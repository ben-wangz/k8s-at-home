package externalcombinations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
	"reflect"
)

func createOrVerifyFixture(ctx context.Context, environment testkit.CaseContext, manifestDir, name string, expected map[string][]byte) (testkit.Object, error) {
	object, err := testkit.KubectlGet(ctx, environment.Namespace, "secret", name)
	if err == nil {
		return verifyFixtureObject(object, environment, name, expected)
	}
	if !testkit.IsNotFound(err) {
		return testkit.Object{}, err
	}
	encodedData := make(map[string]string, len(expected))
	for key, value := range expected {
		encodedData[key] = base64.StdEncoding.EncodeToString(value)
	}
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
		"metadata": map[string]any{
			"name": name, "namespace": environment.Namespace,
			"labels": map[string]string{
				"registry-hub.io/test-id":    environment.RunID,
				"registry-hub.io/instance":   environment.Fullname,
				"registry-hub.io/managed-by": "registry-hub-test-fixture",
			},
		},
		"data": encodedData,
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return testkit.Object{}, err
	}
	path := filepath.Join(manifestDir, name+".json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return testkit.Object{}, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return testkit.Object{}, err
	}
	_, createErr := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "create", "-f", path)
	if createErr != nil {
		object, readErr := testkit.KubectlGet(ctx, environment.Namespace, "secret", name)
		if readErr != nil {
			return testkit.Object{}, createErr
		}
		return verifyFixtureObject(object, environment, name, expected)
	}
	object, err = testkit.KubectlGet(ctx, environment.Namespace, "secret", name)
	if err != nil {
		return testkit.Object{}, err
	}
	return verifyFixtureObject(object, environment, name, expected)
}

func verifyFixtureObject(raw map[string]any, environment testkit.CaseContext, name string, expected map[string][]byte) (testkit.Object, error) {
	metadata := testkit.Map(raw["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["name"]) != name || testkit.String(metadata["uid"]) == "" ||
		testkit.String(metadata["namespace"]) != environment.Namespace || raw["immutable"] != true ||
		testkit.String(raw["type"]) != "Opaque" ||
		testkit.String(labels["registry-hub.io/test-id"]) != environment.RunID ||
		testkit.String(labels["registry-hub.io/instance"]) != environment.Fullname ||
		testkit.String(labels["registry-hub.io/managed-by"]) != "registry-hub-test-fixture" {
		return testkit.Object{}, fmt.Errorf("external fixture Secret %s has unexpected identity or ownership", name)
	}
	object := testkit.Object{Name: name, UID: testkit.String(metadata["uid"]), Metadata: metadata, Data: testkit.Map(raw["data"])}
	actual, err := testkit.DecodeSecretData(object)
	if err != nil {
		return testkit.Object{}, err
	}
	if !reflect.DeepEqual(actual, expected) {
		return testkit.Object{}, fmt.Errorf("external fixture Secret %s data differs from the expected source", name)
	}
	return object, nil
}

func verifyFixturesUnchanged(ctx context.Context, environment testkit.CaseContext, expected []secretSnapshot) error {
	actual := make([]secretSnapshot, 0, len(expected))
	for _, snapshot := range expected {
		object, data, err := support.GetSecret(ctx, environment.Namespace, snapshot.Name)
		if err != nil {
			return err
		}
		actual = append(actual, secretSnapshot{Name: object.Name, UID: object.UID, Hashes: support.SecretHashes(data)})
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("external Secret UID or key data changed")
	}
	return nil
}

func verifyGeneratedUnchanged(ctx context.Context, environment testkit.CaseContext, expected generatedSnapshot) error {
	actual, _, _, err := captureGenerated(ctx, environment)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("generated Secret UID/data or initialization record UID/data changed")
	}
	return nil
}

func upgrade(ctx context.Context, environment testkit.CaseContext, label string, values map[string]any) error {
	valuesPath := filepath.Join(environment.OutputDir, "values-"+label+".json")
	if values != nil {
		encoded, err := json.Marshal(values)
		if err != nil {
			return err
		}
		if err := os.WriteFile(valuesPath, encoded, 0600); err != nil {
			return err
		}
		defer os.Remove(valuesPath)
	} else {
		valuesPath = ""
	}
	arguments := []string{
		"upgrade", environment.Release, environment.ChartDir, "--namespace", environment.Namespace,
		"--reset-values", "--values", environment.BaseValues, "--values", filepath.Join(environment.StateDir, "environment-values.yaml"),
	}
	if valuesPath != "" {
		arguments = append(arguments, "--values", valuesPath)
	}
	arguments = append(arguments, "--wait", "--wait-for-jobs", "--timeout=5m")
	if _, err := testkit.Command(ctx, "helm", arguments...); err != nil {
		return fmt.Errorf("Helm upgrade for configuration mode %s failed: %w", label, err)
	}
	return nil
}

func upgradeWithReuseValues(ctx context.Context, environment testkit.CaseContext) error {
	_, err := testkit.Command(ctx, "helm", "upgrade", environment.Release, environment.ChartDir,
		"--namespace", environment.Namespace, "--reuse-values", "--wait", "--wait-for-jobs", "--timeout=5m")
	if err != nil {
		return fmt.Errorf("Helm upgrade with prior release values failed: %w", err)
	}
	return nil
}

func verifyExternalReferences(ctx context.Context, environment testkit.CaseContext, admin, auth, htpasswd, tls, httpSecret string) error {
	backend, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	registry, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	for _, expected := range []struct {
		deployment map[string]any
		container  string
		env        string
		secret     string
	}{
		{backend, "backend", "REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", auth},
		{backend, "backend", "REGISTRY_HUB_ADMIN_AUTH_CONFIG", admin},
		{registry, "registry", "REGISTRY_HTTP_SECRET", httpSecret},
	} {
		actual, err := envSecretReference(expected.deployment, expected.container, expected.env)
		if err != nil {
			return err
		}
		if actual != expected.secret {
			return fmt.Errorf("deployment %s env %s references Secret %q, expected %q", testkit.String(testkit.Map(expected.deployment["metadata"])["name"]), expected.env, actual, expected.secret)
		}
	}
	for _, expected := range []struct {
		deployment map[string]any
		volume     string
		secret     string
	}{
		{backend, "distribution-ca", tls},
		{registry, "registry-tls", tls},
		{registry, "registry-auth", htpasswd},
	} {
		actual, err := volumeSecretReference(expected.deployment, expected.volume)
		if err != nil {
			return err
		}
		if actual != expected.secret {
			return fmt.Errorf("deployment %s volume %s references Secret %q, expected %q", testkit.String(testkit.Map(expected.deployment["metadata"])["name"]), expected.volume, actual, expected.secret)
		}
	}
	return nil
}

func envSecretReference(deployment map[string]any, containerName, envName string) (string, error) {
	for _, rawContainer := range testkit.Slice(deploymentPodSpec(deployment)["containers"]) {
		container := testkit.Map(rawContainer)
		if testkit.String(container["name"]) != containerName {
			continue
		}
		for _, rawEnv := range testkit.Slice(container["env"]) {
			env := testkit.Map(rawEnv)
			if testkit.String(env["name"]) == envName {
				valueFrom := testkit.Map(env["valueFrom"])
				secretRef := testkit.Map(valueFrom["secretKeyRef"])
				if name := testkit.String(secretRef["name"]); name != "" {
					return name, nil
				}
			}
		}
	}
	return "", fmt.Errorf("deployment is missing Secret-backed env %s on container %s", envName, containerName)
}

func volumeSecretReference(deployment map[string]any, volumeName string) (string, error) {
	for _, rawVolume := range testkit.Slice(deploymentPodSpec(deployment)["volumes"]) {
		volume := testkit.Map(rawVolume)
		if testkit.String(volume["name"]) == volumeName {
			secret := testkit.Map(volume["secret"])
			if name := testkit.String(secret["secretName"]); name != "" {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("deployment is missing Secret volume %s", volumeName)
}
