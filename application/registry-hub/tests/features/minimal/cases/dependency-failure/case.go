package dependencyfailure

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "dependency-failure")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	dependency := environment.DependencyResults["artifact-persistence"]
	repository, err := support.DetailString(dependency, "repository")
	if err != nil {
		return err
	}
	digest, err := support.DetailString(dependency, "manifestDigest")
	if err != nil {
		return err
	}
	diagnostic := "rh-min-" + environment.RunID + "-diag"
	selector, err := backendSelector(ctx, environment)
	if err != nil {
		return err
	}
	service := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": diagnostic, "namespace": environment.Namespace, "labels": map[string]string{"registry-hub.io/managed-by": "registry-hub-test-fixture", "registry-hub.io/test-id": environment.RunID}},
		"spec":     map[string]any{"type": "ClusterIP", "publishNotReadyAddresses": true, "selector": selector, "ports": []map[string]any{{"name": "registry", "port": 5000, "targetPort": "registry", "protocol": "TCP"}, {"name": "admin", "port": 8081, "targetPort": "admin", "protocol": "TCP"}}},
	}
	manifest, err := json.Marshal(service)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	defer func() {
		_, _ = testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "delete", "service", diagnostic, "--ignore-not-found")
	}()
	serviceObject, err := testkit.KubectlGetObject(ctx, environment.Namespace, "service", diagnostic)
	if err != nil {
		return err
	}
	clusterIP := testkit.String(testkit.Map(serviceObject.Raw["spec"])["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return fmt.Errorf("diagnostic Service has no ClusterIP")
	}
	clientURL := "http://" + net.JoinHostPort(clusterIP, "5000")
	adminURL := "http://" + net.JoinHostPort(clusterIP, "8081")
	diagnosticEnvironment := environment
	diagnosticEnvironment.AdminURL = adminURL
	beforeTasks, err := taskIDs(ctx, diagnosticEnvironment)
	if err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "scale", "deployment/"+environment.Fullname+"-registry", "--replicas=0"); err != nil {
		return err
	}
	if err := waitForRegistryEndpoints(ctx, environment, false); err != nil {
		return err
	}
	if err := waitHTTPStatus(ctx, adminURL+"/readyz", http.StatusServiceUnavailable); err != nil {
		return err
	}
	response, err := testkit.Request(ctx, testkit.NewHTTPClient(15*time.Second, nil), http.MethodGet, adminURL+"/healthz", nil, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("backend liveness returned %s during dependency failure", response.Status)
	}
	response.Body.Close()
	client := support.NewRegistryClient(environment)
	client.Base = clientURL
	response, err = client.Get(ctx, repository, "/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusServiceUnavailable {
		response.Body.Close()
		return fmt.Errorf("dependency manifest read returned %s", response.Status)
	}
	response.Body.Close()
	response, err = client.Request(ctx, http.MethodPost, client.Endpoint(repository, "/blobs/uploads/"), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusServiceUnavailable {
		response.Body.Close()
		return fmt.Errorf("dependency upload returned %s", response.Status)
	}
	response.Body.Close()
	response, err = client.Delete(ctx, repository, "/manifests/"+digest)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusServiceUnavailable {
		response.Body.Close()
		return fmt.Errorf("dependency delete returned %s", response.Status)
	}
	response.Body.Close()
	afterTasks, err := taskIDs(ctx, diagnosticEnvironment)
	if err != nil {
		return err
	}
	for id := range afterTasks {
		if _, existed := beforeTasks[id]; !existed {
			return fmt.Errorf("dependency failure created unexpected task %s", id)
		}
	}

	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "scale", "deployment/"+environment.Fullname+"-registry", "--replicas=1"); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname+"-registry", "--timeout=5m"); err != nil {
		return err
	}
	if err := waitForRegistryEndpoints(ctx, environment, true); err != nil {
		return err
	}
	if err := waitHTTPStatus(ctx, environment.AdminURL+"/readyz", http.StatusOK); err != nil {
		return fmt.Errorf("backend readiness did not recover after Distribution restart: %w", err)
	}
	registry := support.NewRegistryClient(environment)
	if _, _, err := support.ReadManifest(ctx, registry, repository, digest, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("fixture did not recover after Distribution restart: %w", err)
	}
	return support.WriteResult(environment, map[string]any{"diagnosticService": diagnostic, "clusterIP": clusterIP, "registryEndpoints": "empty-then-ready", "repository": repository, "manifestDigest": digest})
}

func backendSelector(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return nil, err
	}
	selector := testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"]
	if len(testkit.Map(selector)) == 0 {
		return nil, fmt.Errorf("backend Deployment has no selector")
	}
	return testkit.Map(selector), nil
}

func waitForRegistryEndpoints(ctx context.Context, environment testkit.CaseContext, expectReady bool) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		object, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", environment.Fullname+"-registry")
		if err == nil {
			hasAddress := endpointReady(object)
			if hasAddress == expectReady {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("internal Registry endpoints did not reach expected state ready=%t", expectReady)
		case <-time.After(1 * time.Second):
		}
	}
}

func endpointReady(object map[string]any) bool {
	for _, value := range testkit.Slice(object["subsets"]) {
		if len(testkit.Slice(testkit.Map(value)["addresses"])) > 0 {
			return true
		}
	}
	return false
}

func waitHTTPStatus(ctx context.Context, endpoint string, expected int) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		response, err := testkit.Request(ctx, testkit.NewHTTPClient(10*time.Second, nil), http.MethodGet, endpoint, nil, nil, nil)
		if err == nil {
			status := response.StatusCode
			response.Body.Close()
			if status == expected {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%s did not return %d", endpoint, expected)
		case <-time.After(1 * time.Second):
		}
	}
}

func taskIDs(ctx context.Context, environment testkit.CaseContext) (map[string]struct{}, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=1000", nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, item := range testkit.Slice(page["items"]) {
		if id := testkit.String(testkit.Map(item)["id"]); id != "" {
			result[id] = struct{}{}
		}
	}
	return result, nil
}
