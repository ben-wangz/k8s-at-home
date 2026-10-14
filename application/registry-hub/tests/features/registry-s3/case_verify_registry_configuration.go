package registrys3

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
	"time"
)

func verifyRegistryConfiguration(ctx context.Context, environment testkit.CaseContext, credentialsSecret, tlsSecret string) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	podSpec := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["template"])["spec"])
	var registryContainer map[string]any
	for _, raw := range testkit.Slice(podSpec["containers"]) {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == "registry" {
			registryContainer = container
			break
		}
	}
	if registryContainer == nil || !containerEnvEquals(registryContainer, "SSL_CERT_FILE", registryS3CAPath) || !containerLoadsSecret(registryContainer, credentialsSecret) {
		return fmt.Errorf("Distribution Pod is missing its S3 CA path or credentials Secret")
	}
	if !containerMounts(registryContainer, "registry-s3-ca", "/etc/distribution/s3-ca") || !podVolumeUsesSecret(podSpec, "registry-s3-ca", tlsSecret) {
		return fmt.Errorf("Distribution Pod does not mount the configured S3 CA Secret")
	}
	configMap, err := testkit.KubectlGet(ctx, environment.Namespace, "configmap", environment.Fullname+"-registry-config")
	if err != nil {
		return err
	}
	config := testkit.String(testkit.Map(configMap["data"])["config.yml"])
	for _, expected := range []string{
		"region: \"us-east-1\"", "bucket: \"rh-" + environment.RunID + "\"",
		"rootdirectory: \"" + registryS3RootDirectory + "\"", "secure: true", "forcepathstyle: true",
		"chunksize: 10485760",
		"regionendpoint: \"https://" + environment.Release + registryS3CredentialsSuffix + "." + environment.Namespace + ".svc.cluster.local:8080\"",
	} {
		if !strings.Contains(config, expected) {
			return fmt.Errorf("Distribution S3 config is missing %q", expected)
		}
	}
	return nil
}

func containerEnvEquals(container map[string]any, name, expected string) bool {
	for _, raw := range testkit.Slice(container["env"]) {
		item := testkit.Map(raw)
		if testkit.String(item["name"]) == name {
			return testkit.String(item["value"]) == expected
		}
	}
	return false
}

func containerLoadsSecret(container map[string]any, name string) bool {
	for _, raw := range testkit.Slice(container["envFrom"]) {
		if testkit.String(testkit.Map(testkit.Map(raw)["secretRef"])["name"]) == name {
			return true
		}
	}
	return false
}

func containerMounts(container map[string]any, name, path string) bool {
	for _, raw := range testkit.Slice(container["volumeMounts"]) {
		mount := testkit.Map(raw)
		if testkit.String(mount["name"]) == name && testkit.String(mount["mountPath"]) == path && mount["readOnly"] == true {
			return true
		}
	}
	return false
}

func podVolumeUsesSecret(podSpec map[string]any, name, secretName string) bool {
	for _, raw := range testkit.Slice(podSpec["volumes"]) {
		volume := testkit.Map(raw)
		if testkit.String(volume["name"]) == name && testkit.String(testkit.Map(volume["secret"])["secretName"]) == secretName {
			return true
		}
	}
	return false
}

func s3Endpoint(ctx context.Context, environment testkit.CaseContext, name string) (string, error) {
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", name)
	if err != nil {
		return "", err
	}
	spec := testkit.Map(service["spec"])
	clusterIP := testkit.String(spec["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return "", fmt.Errorf("registry S3 fixture Service has no ClusterIP")
	}
	for _, raw := range testkit.Slice(spec["ports"]) {
		port := testkit.Map(raw)
		if testkit.String(port["name"]) == "s3" && testkit.Int(port["port"]) == 8080 {
			return fmt.Sprintf("https://%s:8080", clusterIP), nil
		}
	}
	return "", fmt.Errorf("registry S3 fixture Service has no S3 port")
}

func newS3Client(ctx context.Context, endpoint, dnsName string, caPEM []byte, accessKey, secretKey string) (*s3.Client, error) {
	return newS3ClientWithRoots(ctx, endpoint, dnsName, caPEM, accessKey, secretKey)
}

func newS3ClientWithRoots(ctx context.Context, endpoint, dnsName string, caPEM []byte, accessKey, secretKey string) (*s3.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("registry S3 fixture CA is invalid")
	}
	return newS3ClientWithPool(ctx, endpoint, dnsName, roots, accessKey, secretKey)
}

func newS3ClientWithPool(ctx context.Context, endpoint, dnsName string, roots *x509.CertPool, accessKey, secretKey string) (*s3.Client, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: dnsName, MinVersion: tls.VersionTLS12}}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithHTTPClient(&http.Client{Transport: transport, Timeout: 30 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(strings.TrimSuffix(endpoint, "/"))
		options.UsePathStyle = true
	}), nil
}

func rejectWrongCredentials(ctx context.Context, endpoint, dnsName string, caPEM []byte, bucket string) error {
	wrong, err := newS3Client(ctx, endpoint, dnsName, caPEM, "invalid-access-key", "invalid-secret-key")
	if err != nil {
		return err
	}
	if _, err := wrong.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1)}); err == nil {
		return fmt.Errorf("registry S3 fixture accepted invalid credentials")
	}
	return nil
}

func uploadArtifact(ctx context.Context, registry *support.RegistryClient, repository, runID string) ([]byte, string, []byte, string, error) {
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest, err := registry.PutBlob(ctx, repository, config)
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("upload OCI config: %w", err)
	}
	seed := []byte("registry-hub-registry-s3:" + runID + ":")
	largeBlob := bytes.Repeat(seed, (12<<20)/len(seed)+1)
	largeBlobDigest, err := registry.PutBlob(ctx, repository, largeBlob)
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("upload large OCI blob: %w", err)
	}
	manifest := support.Manifest{
		SchemaVersion: 2, MediaType: support.OCIManifestMediaType,
		Config: &support.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Size: len(config), Digest: configDigest},
		Layers: []support.Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Size: len(largeBlob), Digest: largeBlobDigest}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", nil, "", err
	}
	response, manifestDigest, err := registry.PutManifest(ctx, repository, "s3-roundtrip", support.OCIManifestMediaType, manifestBytes, nil)
	if err != nil {
		return nil, "", nil, "", err
	}
	response.Body.Close()
	return manifestBytes, manifestDigest, largeBlob, largeBlobDigest, nil
}

func verifyArtifact(ctx context.Context, registry *support.RegistryClient, repository, reference string, manifest []byte, manifestDigest string, largeBlob []byte, largeBlobDigest string) error {
	actualManifest, headers, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualManifest, manifest) || support.Digest(actualManifest) != manifestDigest || !strings.EqualFold(headers.Get("Docker-Content-Digest"), manifestDigest) {
		return fmt.Errorf("Registry S3 manifest bytes or digest changed")
	}
	actualBlob, err := support.ReadBlob(ctx, registry, repository, largeBlobDigest)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualBlob, largeBlob) || support.Digest(actualBlob) != largeBlobDigest {
		return fmt.Errorf("Registry S3 large blob bytes or digest changed")
	}
	return nil
}

func listDistributionObjects(ctx context.Context, client *s3.Client, bucket string) (map[string]string, error) {
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(registryS3RootDirectory + "/"), MaxKeys: aws.Int32(500)})
	objects := make(map[string]string)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list Distribution S3 objects: %w", err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, registryS3RootDirectory+"/") {
				return nil, fmt.Errorf("Distribution S3 object escaped its configured rootDirectory")
			}
			objects[key] = fmt.Sprintf("%s:%d", aws.ToString(object.ETag), aws.ToInt64(object.Size))
		}
	}
	return objects, nil
}

func verifyRegistryObjectPrefix(objects map[string]string) error {
	if len(objects) == 0 {
		return fmt.Errorf("Distribution has not persisted objects to the configured S3 rootDirectory")
	}
	for key := range objects {
		if strings.Contains(key, "registry-hub-backend/") || strings.Contains(key, "/tasks/") || strings.Contains(key, "/cache/") {
			return fmt.Errorf("backend task/cache object appeared in the Distribution S3 namespace")
		}
	}
	return nil
}

func compareRegistryObjectSnapshots(before, after map[string]string) error {
	if len(before) != len(after) {
		return fmt.Errorf("Distribution S3 object count changed across Registry restart")
	}
	for key, value := range before {
		if after[key] != value {
			return fmt.Errorf("Distribution S3 object %s changed across Registry restart", key)
		}
	}
	return nil
}
