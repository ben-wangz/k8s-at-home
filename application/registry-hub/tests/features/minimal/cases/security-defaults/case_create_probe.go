package securitydefaults

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
	"time"
)

func createProbe(ctx context.Context, environment testkit.CaseContext, name string, labels map[string]string, image, nodeName, probeScript string, secretItems []map[string]any) error {
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": environment.Namespace, "labels": labels,
		},
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"nodeName":                     nodeName,
			"restartPolicy":                "Never",
			"containers": []map[string]any{{
				"name": "probe", "image": image, "imagePullPolicy": "IfNotPresent",
				"command": []string{"sh", "-c"}, "args": []string{probeScript},
				"readinessProbe": map[string]any{
					"exec": map[string]any{"command": []string{"sh", "-c", "exit 1"}}, "periodSeconds": 1, "failureThreshold": 1,
				},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts":    []map[string]any{{"name": "registry-credentials", "mountPath": "/run/registry-ca", "readOnly": true}},
			}},
			"volumes": []map[string]any{{"name": "registry-credentials", "secret": map[string]any{"secretName": environment.Fullname + "-registry-internal", "items": secretItems}}},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, content, "kubectl", "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create NetworkPolicy probe %s: %w", name, err)
	}
	return nil
}

func waitProbeStatus(ctx context.Context, environment testkit.CaseContext, name string) (string, error) {
	logs, err := waitProbeEvidence(ctx, environment, name, "probe-status:")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(line, "probe-status:") {
			status := strings.TrimSpace(strings.TrimPrefix(line, "probe-status:"))
			if status != "" {
				return status, nil
			}
		}
	}
	return "", fmt.Errorf("NetworkPolicy probe %s reported no status", name)
}

func waitProbeEvidence(ctx context.Context, environment testkit.CaseContext, name string, markers ...string) (string, error) {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", name, "-c", "probe", "--tail=20")
		if err == nil {
			logs := string(result.Stdout)
			complete := true
			for _, marker := range markers {
				if !strings.Contains(logs, marker) {
					complete = false
					break
				}
			}
			if complete {
				return logs, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("probe %s did not report required evidence", name)
		case <-time.After(1 * time.Second):
		}
	}
}

func verifyProbeNotReady(ctx context.Context, environment testkit.CaseContext, podName string) error {
	endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", environment.Fullname)
	if err != nil {
		return err
	}
	for _, subset := range testkit.Slice(endpoints["subsets"]) {
		for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
			if testkit.String(testkit.Map(testkit.Map(address)["targetRef"])["name"]) == podName {
				return fmt.Errorf("positive NetworkPolicy probe %s entered backend endpoints", podName)
			}
		}
	}
	return nil
}

func deleteNetworkProbe(name, namespace string) {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = testkit.Command(cleanupContext, "kubectl", "-n", namespace, "delete", "pod", name, "--ignore-not-found", "--wait=false")
}

func verifyPolicy(raw map[string]any, environment testkit.CaseContext) error {
	spec := testkit.Map(raw["spec"])
	ingress := testkit.Slice(spec["ingress"])
	if len(ingress) != 1 {
		return fmt.Errorf("internal NetworkPolicy must have one ingress rule")
	}
	rule := testkit.Map(ingress[0])
	from := testkit.Slice(rule["from"])
	if len(from) != 1 {
		return fmt.Errorf("internal NetworkPolicy must have one source selector")
	}
	selector := testkit.Map(testkit.Map(from[0])["podSelector"])["matchLabels"]
	labels := testkit.Map(selector)
	if testkit.String(labels["app.kubernetes.io/name"]) != "registry-hub" || testkit.String(labels["app.kubernetes.io/instance"]) != environment.Release || testkit.String(labels["app.kubernetes.io/component"]) != "backend" {
		return fmt.Errorf("internal NetworkPolicy source is not the backend selector")
	}
	ports := testkit.Slice(rule["ports"])
	if len(ports) != 1 || testkit.Int(testkit.Map(ports[0])["port"]) != 5000 {
		return fmt.Errorf("internal NetworkPolicy does not restrict port 5000")
	}
	return nil
}

func waitForFailureTask(ctx context.Context, environment testkit.CaseContext, authority, repository string) (string, string, bool, error) {
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=100", nil, nil)
		if err != nil {
			return "", "", false, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return "", "", false, err
		}
		for _, item := range testkit.Slice(page["items"]) {
			task := testkit.Map(item)
			if testkit.String(task["authority"]) == authority && testkit.String(task["repository"]) == repository && testkit.String(task["status"]) == "failed" {
				retryable, _ := task["retryable"].(bool)
				return testkit.String(task["id"]), testkit.String(task["status"]), retryable, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", "", false, ctx.Err()
		case <-deadline.C:
			return "", "", false, fmt.Errorf("SSRF failure task was not persisted")
		case <-time.After(1 * time.Second):
		}
	}
}
