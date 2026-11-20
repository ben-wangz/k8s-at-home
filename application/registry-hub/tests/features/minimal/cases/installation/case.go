package installation

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "installation")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	clientStatus := http.StatusOK
	if _, err := testkit.Command(ctx, "helm", "status", environment.Release, "--namespace", environment.Namespace); err != nil {
		return err
	}
	for _, name := range []string{environment.Fullname, environment.Fullname + "-registry"} {
		object, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", name)
		if err != nil {
			return err
		}
		status := testkit.Map(object["status"])
		if testkit.Int(status["readyReplicas"]) != 1 || testkit.Int(status["availableReplicas"]) != 1 {
			return fmt.Errorf("deployment %s is not Ready", name)
		}
		if name == environment.Fullname {
			if err := verifyBackendImages(object, environment.ImageReference); err != nil {
				return err
			}
			clientStatus = expectedClientRegistryStatus(object)
		}
	}
	if err := verifyInitializationJob(ctx, environment); err != nil {
		return err
	}
	storageClass, err := verifyPVCs(ctx, environment)
	if err != nil {
		return err
	}
	client := testkit.NewHTTPClient(10*time.Second, nil)
	response, err := testkit.Request(ctx, client, "GET", environment.ClientURL+"/v2/", nil, nil, nil)
	if err != nil {
		return fmt.Errorf("client Registry service is not reachable: %w", err)
	}
	if err := testkit.ExpectStatus(response, clientStatus); err != nil {
		return err
	}
	return writeResult(environment, map[string]any{"clientRegistryStatus": clientStatus, "defaultStorageClass": storageClass})
}

func expectedClientRegistryStatus(deployment map[string]any) int {
	podSpec := testkit.Map(testkit.Map(deployment["spec"])["template"])
	spec := testkit.Map(podSpec["spec"])
	for _, rawContainer := range testkit.Slice(spec["containers"]) {
		container := testkit.Map(rawContainer)
		for _, rawEnv := range testkit.Slice(container["env"]) {
			if testkit.String(testkit.Map(rawEnv)["name"]) == "REGISTRY_HUB_CLIENT_AUTH_CONFIG" {
				return http.StatusUnauthorized
			}
		}
	}
	return http.StatusOK
}

func verifyBackendImages(deployment map[string]any, expected string) error {
	podSpec := testkit.Map(testkit.Map(deployment["spec"])["template"])
	spec := testkit.Map(podSpec["spec"])
	images := make([]string, 0)
	for _, field := range []string{"initContainers", "containers"} {
		for _, raw := range testkit.Slice(spec[field]) {
			images = append(images, testkit.String(testkit.Map(raw)["image"]))
		}
	}
	if len(images) < 2 {
		return fmt.Errorf("backend deployment has no initialization wait containers")
	}
	for _, image := range images {
		if image != expected {
			return fmt.Errorf("backend deployment references an unexpected image digest")
		}
	}
	return nil
}

func verifyInitializationJob(ctx context.Context, environment testkit.CaseContext) error {
	objects, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", "app.kubernetes.io/instance="+environment.Release+",app.kubernetes.io/component=internal-initialization")
	if err != nil {
		return err
	}
	if len(objects) != 1 || testkit.Int(testkit.Map(objects[0].Raw["status"])["succeeded"]) != 1 {
		return fmt.Errorf("initialization Job did not complete successfully")
	}
	return nil
}

func verifyPVCs(ctx context.Context, environment testkit.CaseContext) (string, error) {
	objects, err := testkit.KubectlList(ctx, environment.Namespace, "get", "persistentvolumeclaims", "-l", "app.kubernetes.io/instance="+environment.Release)
	if err != nil {
		return "", err
	}
	valuesOutput, err := testkit.Command(ctx, "helm", "get", "values", environment.Release, "--namespace", environment.Namespace, "--all", "--output", "json")
	if err != nil {
		return "", err
	}
	var values map[string]any
	if err := json.Unmarshal(valuesOutput.Stdout, &values); err != nil {
		return "", err
	}
	expected, err := expectedPersistentClaims(values, environment.Fullname)
	if err != nil {
		return "", err
	}
	var storageClass string
	for _, object := range objects {
		spec := testkit.Map(object.Raw["spec"])
		requests := testkit.Map(spec["resources"])
		storage := testkit.String(testkit.Map(requests["requests"])["storage"])
		phase := testkit.String(testkit.Map(object.Raw["status"])["phase"])
		class := testkit.String(spec["storageClassName"])
		if expected[object.Name] == "" || storage != expected[object.Name] || phase != "Bound" || class == "" {
			return "", fmt.Errorf("PVC %s does not match the default storage contract", object.Name)
		}
		if storageClass != "" && storageClass != class {
			return "", fmt.Errorf("PVCs were assigned different StorageClasses")
		}
		storageClass = class
		delete(expected, object.Name)
	}
	if len(expected) != 0 {
		return "", fmt.Errorf("expected PVCs for scenario %s were not created", environment.ScenarioID)
	}
	return storageClass, nil
}

func writeResult(environment testkit.CaseContext, details map[string]any) error {
	now := time.Now().UTC()
	result := testkit.CaseResultFile{CaseID: environment.CaseID, Status: "passed", StartedAt: now, Finished: now, Details: details}
	return testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "result.json"), result)
}
