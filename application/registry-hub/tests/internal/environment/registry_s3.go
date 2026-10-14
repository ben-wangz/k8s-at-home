package environment

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

const registryS3RootDirectory = "registry-hub-distribution"

type RegistryS3Fixture struct {
	CredentialsSecret string
	CASecret          string
	SecretSHA256      string
	Endpoint          string
	Bucket            string
}

func (environment Environment) PrepareRegistryS3Fixture(ctx context.Context, runID, fullname, expectedDigest string) (RegistryS3Fixture, error) {
	release := strings.TrimSuffix(fullname, "-registry-hub")
	if release == fullname || release == "" {
		return RegistryS3Fixture{}, fmt.Errorf("registry S3 fixture has an invalid release identity")
	}
	name := release + "-external-registry-s3"
	credentialsSecret, credentialsData, err := environment.prepareRegistryS3Credentials(ctx, runID, fullname, name, expectedDigest != "")
	if err != nil {
		return RegistryS3Fixture{}, err
	}
	tlsSecretName := name + "-tls"
	dnsName := name + "." + Namespace + ".svc.cluster.local"
	tlsSecret, tlsData, err := environment.prepareRegistryS3TLSSecret(ctx, runID, fullname, tlsSecretName, dnsName, expectedDigest != "")
	if err != nil {
		return RegistryS3Fixture{}, err
	}
	digest := digestRegistryS3Fixture(credentialsData, tlsData)
	if expectedDigest != "" && expectedDigest != digest {
		return RegistryS3Fixture{}, fmt.Errorf("registry S3 fixture changed for a preserved run")
	}
	if err := environment.ensureRegistryS3Service(ctx, name, runID, fullname, expectedDigest != ""); err != nil {
		return RegistryS3Fixture{}, err
	}
	if err := environment.ensureRegistryS3Pod(ctx, name, runID, fullname, credentialsSecret.Name, tlsSecret.Name, expectedDigest != ""); err != nil {
		return RegistryS3Fixture{}, err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=15m"); err != nil {
		return RegistryS3Fixture{}, fmt.Errorf("registry S3 fixture pod did not become Ready: %w", err)
	}
	service, err := environment.Service(ctx, name, "s3")
	if err != nil {
		return RegistryS3Fixture{}, err
	}
	endpoint := "https://" + service.Address
	client, err := newRegistryS3FixtureClient(ctx, endpoint, dnsName, tlsData["ca.crt"], string(credentialsData["AWS_ACCESS_KEY_ID"]), string(credentialsData["AWS_SECRET_ACCESS_KEY"]))
	if err != nil {
		return RegistryS3Fixture{}, err
	}
	bucket := "rh-" + runID
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, createErr := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); createErr != nil {
			if _, headErr := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); headErr != nil {
				return RegistryS3Fixture{}, fmt.Errorf("create registry S3 bucket: %w", createErr)
			}
		}
	}
	return RegistryS3Fixture{
		CredentialsSecret: credentialsSecret.Name,
		CASecret:          tlsSecret.Name,
		SecretSHA256:      digest,
		Endpoint:          "https://" + dnsName + ":8080",
		Bucket:            bucket,
	}, nil
}

func (environment Environment) prepareRegistryS3Credentials(ctx context.Context, runID, fullname, name string, preserved bool) (testkit.Object, map[string][]byte, error) {
	secret, data, err := environment.registryS3Secret(ctx, runID, fullname, name, preserved, func() (map[string][]byte, error) {
		accessKey, err := randomS3Credential()
		if err != nil {
			return nil, err
		}
		secretKey, err := randomS3Credential()
		if err != nil {
			return nil, err
		}
		return map[string][]byte{"AWS_ACCESS_KEY_ID": []byte(accessKey), "AWS_SECRET_ACCESS_KEY": []byte(secretKey)}, nil
	})
	if err != nil {
		return testkit.Object{}, nil, err
	}
	if err := validateBackendS3Credentials(data); err != nil {
		return testkit.Object{}, nil, fmt.Errorf("registry S3 credentials Secret %s is invalid: %w", name, err)
	}
	return secret, data, nil
}

func (environment Environment) prepareRegistryS3TLSSecret(ctx context.Context, runID, fullname, name, dnsName string, preserved bool) (testkit.Object, map[string][]byte, error) {
	secret, data, err := environment.registryS3Secret(ctx, runID, fullname, name, preserved, func() (map[string][]byte, error) {
		return newRegistryS3TLSMaterial(runID, dnsName)
	})
	if err != nil {
		return testkit.Object{}, nil, err
	}
	if len(data) != 3 || len(data["tls.crt"]) == 0 || len(data["tls.key"]) == 0 || len(data["ca.crt"]) == 0 {
		return testkit.Object{}, nil, fmt.Errorf("registry S3 TLS Secret %s has an unexpected shape", name)
	}
	if err := validateRegistryS3TLSMaterial(data, dnsName); err != nil {
		return testkit.Object{}, nil, fmt.Errorf("registry S3 TLS Secret %s is invalid: %w", name, err)
	}
	return secret, data, nil
}

func (environment Environment) registryS3Secret(ctx context.Context, runID, fullname, name string, preserved bool, generate func() (map[string][]byte, error)) (testkit.Object, map[string][]byte, error) {
	object, err := testkit.KubectlGet(ctx, Namespace, "secret", name)
	if err != nil && !testkit.IsNotFound(err) {
		return testkit.Object{}, nil, err
	}
	if testkit.IsNotFound(err) {
		if preserved {
			return testkit.Object{}, nil, fmt.Errorf("registry S3 fixture Secret %s is missing for a preserved run", name)
		}
		data, err := generate()
		if err != nil {
			return testkit.Object{}, nil, err
		}
		encodedData := make(map[string]string, len(data))
		for key, value := range data {
			encodedData[key] = base64.StdEncoding.EncodeToString(value)
		}
		payload := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata": map[string]any{"name": name, "namespace": Namespace, "labels": fixtureLabels(runID, fullname, name)},
			"data":     encodedData,
		}
		if err := createFixture(ctx, payload); err != nil {
			return testkit.Object{}, nil, fmt.Errorf("create registry S3 fixture Secret %s: %w", name, err)
		}
		object, err = testkit.KubectlGet(ctx, Namespace, "secret", name)
		if err != nil {
			return testkit.Object{}, nil, err
		}
	}
	secret := objectForSecret(object)
	if err := validateFixtureOwnership(object, name, runID, fullname); err != nil {
		return testkit.Object{}, nil, err
	}
	if secret.Type != "Opaque" || !secret.Immutable {
		return testkit.Object{}, nil, fmt.Errorf("registry S3 fixture Secret %s must be immutable and opaque", name)
	}
	data, err := testkit.DecodeSecretData(secret)
	return secret, data, err
}

func (environment Environment) ensureRegistryS3Service(ctx context.Context, name, runID, fullname string, preserved bool) error {
	service, err := testkit.KubectlGet(ctx, Namespace, "service", name)
	if err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if testkit.IsNotFound(err) {
		if preserved {
			return fmt.Errorf("registry S3 fixture Service %s is missing for a preserved run", name)
		}
		payload := map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": name, "namespace": Namespace, "labels": fixtureLabels(runID, fullname, name)},
			"spec": map[string]any{
				"type": "ClusterIP", "selector": map[string]string{"registry-hub.io/fixture": name},
				"ports": []map[string]any{{"name": "s3", "port": 8080, "targetPort": 8080, "protocol": "TCP"}},
			},
		}
		if err := createFixture(ctx, payload); err != nil {
			return err
		}
		service, err = testkit.KubectlGet(ctx, Namespace, "service", name)
		if err != nil {
			return err
		}
	}
	if err := validateFixtureOwnership(service, name, runID, fullname); err != nil {
		return err
	}
	spec := testkit.Map(service["spec"])
	ports := testkit.Slice(spec["ports"])
	if testkit.String(spec["type"]) != "ClusterIP" || len(ports) != 1 || testkit.String(testkit.Map(ports[0])["name"]) != "s3" || testkit.Int(testkit.Map(ports[0])["port"]) != 8080 {
		return fmt.Errorf("registry S3 fixture Service %s has an unexpected configuration", name)
	}
	return nil
}

func (environment Environment) ensureRegistryS3Pod(ctx context.Context, name, runID, fullname, credentialsSecret, tlsSecret string, preserved bool) error {
	object, err := testkit.KubectlGet(ctx, Namespace, "pod", name)
	if err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if testkit.IsNotFound(err) {
		if preserved {
			return fmt.Errorf("registry S3 fixture Pod %s is missing for a preserved run", name)
		}
		if err := createFixture(ctx, registryS3Pod(name, runID, fullname, credentialsSecret, tlsSecret)); err != nil {
			return err
		}
		object, err = testkit.KubectlGet(ctx, Namespace, "pod", name)
		if err != nil {
			return err
		}
	}
	if err := validateFixtureOwnership(object, name, runID, fullname); err != nil {
		return err
	}
	containers := testkit.Slice(testkit.Map(object["spec"])["containers"])
	if len(containers) != 1 {
		return fmt.Errorf("registry S3 fixture Pod %s has an unexpected container count", name)
	}
	container := testkit.Map(containers[0])
	args := testkit.Slice(container["args"])
	expectedArgs := []string{"--access-log", "stdout", "--quiet", "--cert", "/certs/tls.crt", "--key", "/certs/tls.key", "--port", ":8080", "--region", "us-east-1", "posix", "/data"}
	if testkit.String(container["name"]) != "versitygw" || testkit.String(container["image"]) != backendS3Image || len(args) != len(expectedArgs) {
		return fmt.Errorf("registry S3 fixture Pod %s has an unexpected image or arguments", name)
	}
	for index, expected := range expectedArgs {
		if testkit.String(args[index]) != expected {
			return fmt.Errorf("registry S3 fixture Pod %s has unexpected arguments", name)
		}
	}
	return nil
}
