package environment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

const backendS3Image = "container-registry-mirror.internal.home.geekcity.tech:32443/ghcr.io/versity/versitygw:v1.8.0@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499"

type BackendS3Fixture struct {
	SecretName   string
	SecretSHA256 string
	Endpoint     string
	Bucket       string
}

func (environment Environment) PrepareBackendS3Fixture(ctx context.Context, runID, fullname, expectedDigest string) (BackendS3Fixture, error) {
	release := strings.TrimSuffix(fullname, "-registry-hub")
	if release == fullname || release == "" {
		return BackendS3Fixture{}, fmt.Errorf("backend S3 fixture has an invalid release identity")
	}
	name := release + "-external-backend-s3"
	secret, data, digest, err := environment.prepareBackendS3Secret(ctx, runID, fullname, name, expectedDigest)
	if err != nil {
		return BackendS3Fixture{}, err
	}
	if err := environment.ensureBackendS3Service(ctx, name, runID, fullname, expectedDigest); err != nil {
		return BackendS3Fixture{}, err
	}
	if err := environment.ensureBackendS3Pod(ctx, name, runID, fullname, secret.Name, expectedDigest); err != nil {
		return BackendS3Fixture{}, err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=15m"); err != nil {
		return BackendS3Fixture{}, fmt.Errorf("backend S3 fixture pod did not become Ready: %w", err)
	}
	service, err := environment.Service(ctx, name, "s3")
	if err != nil {
		return BackendS3Fixture{}, err
	}
	accessKey := string(data["AWS_ACCESS_KEY_ID"])
	secretKey := string(data["AWS_SECRET_ACCESS_KEY"])
	bucket := "rh-" + runID
	if err := ensureS3Bucket(ctx, "http://"+service.Address, bucket, accessKey, secretKey); err != nil {
		return BackendS3Fixture{}, err
	}
	if err := verifyS3ConditionalOperations(ctx, "http://"+service.Address, bucket, accessKey, secretKey); err != nil {
		return BackendS3Fixture{}, err
	}
	return BackendS3Fixture{
		SecretName: secret.Name, SecretSHA256: digest,
		Endpoint: "http://" + name + "." + Namespace + ".svc.cluster.local:8080",
		Bucket:   bucket,
	}, nil
}

func (environment Environment) prepareBackendS3Secret(ctx context.Context, runID, fullname, name, expectedDigest string) (testkit.Object, map[string][]byte, string, error) {
	object, err := testkit.KubectlGet(ctx, Namespace, "secret", name)
	if err != nil && !testkit.IsNotFound(err) {
		return testkit.Object{}, nil, "", err
	}
	if testkit.IsNotFound(err) && expectedDigest != "" {
		return testkit.Object{}, nil, "", fmt.Errorf("backend S3 fixture Secret %s is missing for a preserved run", name)
	}
	if testkit.IsNotFound(err) {
		accessKey, err := randomS3Credential()
		if err != nil {
			return testkit.Object{}, nil, "", err
		}
		secretKey, err := randomS3Credential()
		if err != nil {
			return testkit.Object{}, nil, "", err
		}
		data := map[string][]byte{"AWS_ACCESS_KEY_ID": []byte(accessKey), "AWS_SECRET_ACCESS_KEY": []byte(secretKey)}
		if err := validateBackendS3Credentials(data); err != nil {
			return testkit.Object{}, nil, "", err
		}
		encodedData := make(map[string]string, len(data))
		for key, value := range data {
			encodedData[key] = base64.StdEncoding.EncodeToString(value)
		}
		payload := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata": map[string]any{
				"name": name, "namespace": Namespace,
				"labels": fixtureLabels(runID, fullname, name),
			},
			"data": encodedData,
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return testkit.Object{}, nil, "", err
		}
		if _, createErr := testkit.CommandInput(ctx, encoded, "kubectl", "create", "-f", "-"); createErr != nil {
			object, err = testkit.KubectlGet(ctx, Namespace, "secret", name)
			if err != nil {
				return testkit.Object{}, nil, "", createErr
			}
		} else {
			object, err = testkit.KubectlGet(ctx, Namespace, "secret", name)
			if err != nil {
				return testkit.Object{}, nil, "", err
			}
		}
	}
	secret := objectForSecret(object)
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if secret.Name != name || secret.UID == "" || secret.Type != "Opaque" || secret.Immutable != true ||
		testkit.String(labels["registry-hub.io/test-id"]) != runID ||
		testkit.String(labels["registry-hub.io/instance"]) != fullname ||
		testkit.String(labels["registry-hub.io/managed-by"]) != "registry-hub-test-fixture" {
		return testkit.Object{}, nil, "", fmt.Errorf("backend S3 fixture Secret %s has unexpected ownership or type", name)
	}
	data, err := testkit.DecodeSecretData(secret)
	if err != nil {
		return testkit.Object{}, nil, "", err
	}
	if err := validateBackendS3Credentials(data); err != nil {
		return testkit.Object{}, nil, "", fmt.Errorf("backend S3 fixture Secret %s is invalid: %w", name, err)
	}
	digest := digestS3Credentials(data)
	if expectedDigest != "" && expectedDigest != digest {
		return testkit.Object{}, nil, "", fmt.Errorf("backend S3 fixture Secret %s changed for a preserved run", name)
	}
	return secret, data, digest, nil
}

func objectForSecret(object map[string]any) testkit.Object {
	metadata := testkit.Map(object["metadata"])
	labels := make(map[string]string)
	for key, value := range testkit.Map(metadata["labels"]) {
		if text, ok := value.(string); ok {
			labels[key] = text
		}
	}
	return testkit.Object{
		Kind: testkit.String(object["kind"]), Type: testkit.String(object["type"]),
		Immutable: object["immutable"] == true, Name: testkit.String(metadata["name"]), UID: testkit.String(metadata["uid"]),
		Labels: labels, Data: testkit.Map(object["data"]), Raw: object,
	}
}

func (environment Environment) ensureBackendS3Service(ctx context.Context, name, runID, fullname, expectedDigest string) error {
	service, err := testkit.KubectlGet(ctx, Namespace, "service", name)
	if err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if testkit.IsNotFound(err) {
		if expectedDigest != "" {
			return fmt.Errorf("backend S3 fixture Service %s is missing for a preserved run", name)
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
	if len(testkit.Slice(spec["ports"])) != 1 || testkit.String(testkit.Map(testkit.Slice(spec["ports"])[0])["name"]) != "s3" {
		return fmt.Errorf("backend S3 fixture Service %s has an unexpected port", name)
	}
	return nil
}

func (environment Environment) ensureBackendS3Pod(ctx context.Context, name, runID, fullname, secretName, expectedDigest string) error {
	object, err := testkit.KubectlGet(ctx, Namespace, "pod", name)
	if err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if testkit.IsNotFound(err) {
		if expectedDigest != "" {
			return fmt.Errorf("backend S3 fixture Pod %s is missing for a preserved run", name)
		}
		payload := backendS3Pod(name, runID, fullname, secretName)
		if err := createFixture(ctx, payload); err != nil {
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
		return fmt.Errorf("backend S3 fixture Pod %s has an unexpected container count", name)
	}
	container := testkit.Map(containers[0])
	args := testkit.Slice(container["args"])
	expectedArgs := []string{"--access-log", "stdout", "--quiet", "--port", ":8080", "--region", "us-east-1", "posix", "/data"}
	argsMatch := len(args) == len(expectedArgs)
	for index, expected := range expectedArgs {
		if !argsMatch || testkit.String(args[index]) != expected {
			argsMatch = false
			break
		}
	}
	if testkit.String(container["name"]) != "versitygw" || testkit.String(container["image"]) != backendS3Image || !argsMatch {
		return fmt.Errorf("backend S3 fixture Pod %s has an unexpected image or arguments", name)
	}
	return nil
}
