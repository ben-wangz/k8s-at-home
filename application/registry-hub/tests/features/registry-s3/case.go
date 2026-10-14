package registrys3

import (
	"context"
	"crypto/x509"
	"embed"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/url"
	"strconv"
	"strings"
)

//go:embed case.json
var metadata embed.FS

const (
	registryS3CredentialsSuffix = "-external-registry-s3"
	registryS3TLSSecretSuffix   = "-external-registry-s3-tls"
	registryS3RootDirectory     = "registry-hub-distribution"
	registryS3CAPath            = "/etc/distribution/s3-ca/ca.crt"
)

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-registry-s3")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	credentialsSecret := environment.Release + registryS3CredentialsSuffix
	tlsSecret := environment.Release + registryS3TLSSecretSuffix
	credentialsObject, credentialsData, err := support.GetSecret(ctx, environment.Namespace, credentialsSecret)
	if err != nil {
		return err
	}
	if err := validateFixtureSecret(credentialsObject, environment, credentialsSecret, credentialsData, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}); err != nil {
		return err
	}
	tlsObject, tlsData, err := support.GetSecret(ctx, environment.Namespace, tlsSecret)
	if err != nil {
		return err
	}
	if err := validateFixtureSecret(tlsObject, environment, tlsSecret, tlsData, []string{"ca.crt", "tls.crt", "tls.key"}); err != nil {
		return err
	}
	if err := verifyRegistryConfiguration(ctx, environment, credentialsSecret, tlsSecret); err != nil {
		return err
	}
	serviceName := credentialsSecret
	dnsName := serviceName + "." + environment.Namespace + ".svc.cluster.local"
	endpoint, err := s3Endpoint(ctx, environment, serviceName)
	if err != nil {
		return err
	}
	client, err := newS3Client(ctx, endpoint, dnsName, tlsData["ca.crt"], string(credentialsData["AWS_ACCESS_KEY_ID"]), string(credentialsData["AWS_SECRET_ACCESS_KEY"]))
	if err != nil {
		return err
	}
	wrongCA, err := newS3ClientWithPool(ctx, endpoint, dnsName, x509.NewCertPool(), string(credentialsData["AWS_ACCESS_KEY_ID"]), string(credentialsData["AWS_SECRET_ACCESS_KEY"]))
	if err != nil {
		return err
	}
	if _, err := wrongCA.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("rh-" + environment.RunID)}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		return fmt.Errorf("S3 fixture did not reject an untrusted CA")
	}
	bucket := "rh-" + environment.RunID
	if err := rejectWrongCredentials(ctx, endpoint, dnsName, tlsData["ca.crt"], bucket); err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	repository := support.LocalRepository(environment.CaseID, environment.RunID)
	manifest, manifestDigest, largeBlob, largeBlobDigest, err := uploadArtifact(ctx, registry, repository, environment.RunID)
	if err != nil {
		return err
	}
	fixtureLogs, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+serviceName, "-c", "versitygw")
	if err != nil {
		return err
	}
	if strings.Contains(string(fixtureLogs.Stdout), string(credentialsData["AWS_SECRET_ACCESS_KEY"])) {
		return fmt.Errorf("registry S3 fixture access log exposed the secret access key")
	}
	if err := verifyArtifact(ctx, registry, repository, "s3-roundtrip", manifest, manifestDigest, largeBlob, largeBlobDigest); err != nil {
		return err
	}
	before, err := listDistributionObjects(ctx, client, bucket)
	if err != nil {
		return err
	}
	if err := verifyRegistryObjectPrefix(before); err != nil {
		return err
	}
	multipartObjectKey, err := findObjectBySize(before, int64(len(largeBlob)))
	if err != nil {
		return err
	}
	multipartParts, err := findDistributionMultipartUpload(fixtureLogs.Stdout, repository)
	if err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+environment.Fullname+"-registry"); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname+"-registry", "--timeout=5m"); err != nil {
		return err
	}
	if err := verifyArtifact(ctx, registry, repository, "s3-roundtrip", manifest, manifestDigest, largeBlob, largeBlobDigest); err != nil {
		return fmt.Errorf("artifact did not survive Registry restart: %w", err)
	}
	after, err := listDistributionObjects(ctx, client, bucket)
	if err != nil {
		return err
	}
	if err := compareRegistryObjectSnapshots(before, after); err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"credentialsSecret": credentialsSecret, "credentialsSHA256": support.SecretHashes(credentialsData),
		"caSecret": tlsSecret, "tlsSHA256": support.SecretHashes(tlsData),
		"bucket": bucket, "rootDirectory": registryS3RootDirectory,
		"s3ObjectCountBefore": len(before), "s3ObjectCountAfter": len(after), "s3ObjectKeysSHA256": objectSnapshotDigest(after),
		"manifestDigest": manifestDigest, "largeBlobDigest": largeBlobDigest, "largeBlobSize": len(largeBlob),
		"multipartObjectKey": multipartObjectKey, "multipartParts": multipartParts,
		"restartReadback": "passed",
	})
}

func findObjectBySize(objects map[string]string, expectedSize int64) (string, error) {
	var candidateKey string
	for key, value := range objects {
		separator := strings.LastIndexByte(value, ':')
		if separator < 0 {
			continue
		}
		size, err := strconv.ParseInt(value[separator+1:], 10, 64)
		if err != nil || size != expectedSize {
			continue
		}
		candidateKey = key
	}
	if candidateKey != "" {
		return candidateKey, nil
	}
	return "", fmt.Errorf("Distribution S3 object with size %d was not found", expectedSize)
}

func findDistributionMultipartUpload(accessLog []byte, repository string) (int, error) {
	keyPrefix := registryS3RootDirectory + "/docker/registry/v2/repositories/" + repository + "/_uploads/"
	type upload struct {
		completed bool
		parts     map[int]bool
	}
	uploads := make(map[string]*upload)
	createdPaths := make(map[string]bool)
	for _, line := range strings.Split(string(accessLog), "\n") {
		operation, key, query, status, ok := parseVersityRequest(line)
		if !ok || status < 200 || status >= 300 || !strings.HasPrefix(key, keyPrefix) || !strings.HasSuffix(key, "/data") {
			continue
		}
		switch operation {
		case "s3_CreateMultipartUpload":
			createdPaths[key] = true
		case "s3_UploadPart":
			uploadID := query.Get("uploadId")
			part, err := strconv.Atoi(query.Get("partNumber"))
			if uploadID != "" && err == nil && part > 0 {
				sessionKey := key + "\x00" + uploadID
				current := uploads[sessionKey]
				if current == nil {
					current = &upload{parts: make(map[int]bool)}
					uploads[sessionKey] = current
				}
				current.parts[part] = true
			}
		case "s3_CompleteMultipartUpload":
			uploadID := query.Get("uploadId")
			if uploadID != "" {
				sessionKey := key + "\x00" + uploadID
				current := uploads[sessionKey]
				if current == nil {
					current = &upload{parts: make(map[int]bool)}
					uploads[sessionKey] = current
				}
				current.completed = true
			}
		}
	}
	for key, current := range uploads {
		separator := strings.IndexByte(key, '\x00')
		path := key[:separator]
		if createdPaths[path] && current.completed && current.parts[1] && current.parts[2] && len(current.parts) == 2 {
			return len(current.parts), nil
		}
	}
	return 0, fmt.Errorf("VersityGW access log does not show a completed two-part upload for repository %s", repository)
}

func parseVersityRequest(line string) (string, string, url.Values, int, bool) {
	fields := strings.Fields(line)
	operationIndex := -1
	for index, field := range fields {
		if strings.HasPrefix(field, "s3_") {
			operationIndex = index
			break
		}
	}
	if operationIndex < 0 || operationIndex+3 >= len(fields) {
		return "", "", nil, 0, false
	}
	status, err := strconv.Atoi(fields[operationIndex+3])
	if err != nil {
		return "", "", nil, 0, false
	}
	target, err := url.ParseRequestURI(fields[operationIndex+2])
	if err != nil {
		return "", "", nil, 0, false
	}
	return fields[operationIndex], fields[operationIndex+1], target.Query(), status, true
}

func validateFixtureSecret(secret testkit.Object, environment testkit.CaseContext, name string, data map[string][]byte, expectedKeys []string) error {
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" || len(data) != len(expectedKeys) {
		return fmt.Errorf("registry S3 fixture Secret %s ownership or shape is invalid", name)
	}
	for _, key := range expectedKeys {
		if len(data[key]) == 0 {
			return fmt.Errorf("registry S3 fixture Secret %s is missing key %s", name, key)
		}
	}
	return nil
}
