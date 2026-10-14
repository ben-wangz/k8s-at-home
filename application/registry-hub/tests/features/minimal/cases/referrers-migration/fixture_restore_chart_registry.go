package referrersmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"time"
)

func restoreChartRegistry(ctx context.Context, fixture zotFixture) error {
	var result error
	if err := patchServiceSelector(ctx, fixture.namespace, fixture.serviceName, fixture.originalSelector); err != nil {
		result = errors.Join(result, fmt.Errorf("restore Registry Service selector: %w", err))
	}
	if err := scaleRegistry(ctx, fixture, fixture.replicas); err != nil {
		result = errors.Join(result, fmt.Errorf("restore Registry Deployment replicas: %w", err))
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", fixture.namespace, "rollout", "status", "deployment/"+fixture.deploymentName, "--timeout=180s"); err != nil {
		result = errors.Join(result, fmt.Errorf("wait for restored Registry Deployment: %w", err))
	}
	return result
}

func registrySecretVolumes(deployment map[string]any) map[string]secretVolume {
	podSpec := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["template"])["spec"])
	result := make(map[string]secretVolume)
	for _, raw := range testkit.Slice(podSpec["volumes"]) {
		volume := testkit.Map(raw)
		name := testkit.String(volume["name"])
		secret := testkit.Map(volume["secret"])
		if name != "registry-tls" && name != "registry-auth" {
			continue
		}
		entry := secretVolume{secretName: testkit.String(secret["secretName"]), keys: make(map[string]string)}
		for _, rawItem := range testkit.Slice(secret["items"]) {
			item := testkit.Map(rawItem)
			entry.keys[testkit.String(item["path"])] = testkit.String(item["key"])
		}
		result[name] = entry
	}
	return result
}

func zotPod(environment testkit.CaseContext, fixture zotFixture, labels map[string]any, tlsSecret, certKey, keyKey, authSecret, htpasswdKey string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": fixture.podName, "namespace": environment.Namespace, "labels": labels},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Always",
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []map[string]any{{
				"name": "registry", "image": zotFixtureImage, "imagePullPolicy": "IfNotPresent",
				"args":            []string{"serve", "/etc/zot/config.json"},
				"ports":           []map[string]any{{"name": "https-registry", "containerPort": 5000, "protocol": "TCP"}},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]string{"cpu": "500m", "memory": "512Mi"}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string][]string{"drop": {"ALL"}}},
				"readinessProbe":  map[string]any{"tcpSocket": map[string]any{"port": "https-registry"}, "periodSeconds": 2, "failureThreshold": 30},
				"volumeMounts": []map[string]any{
					{"name": "configuration", "mountPath": "/etc/zot/config.json", "subPath": "config.json", "readOnly": true},
					{"name": "registry-tls", "mountPath": "/etc/zot/tls", "readOnly": true},
					{"name": "registry-auth", "mountPath": "/etc/zot/auth", "readOnly": true},
					{"name": "data", "mountPath": "/var/lib/zot"},
					{"name": "tmp", "mountPath": "/tmp"},
				},
			}},
			"volumes": []map[string]any{
				{"name": "configuration", "configMap": map[string]any{"name": fixture.configMapName}},
				{"name": "registry-tls", "secret": map[string]any{"secretName": tlsSecret, "items": []map[string]string{{"key": certKey, "path": "tls.crt"}, {"key": keyKey, "path": "tls.key"}}}},
				{"name": "registry-auth", "secret": map[string]any{"secretName": authSecret, "items": []map[string]string{{"key": htpasswdKey, "path": "htpasswd"}}}},
				{"name": "data", "emptyDir": map[string]any{}},
				{"name": "tmp", "emptyDir": map[string]any{}},
			},
		},
	}
}

func fixtureLabels(environment testkit.CaseContext) map[string]string {
	return map[string]string{
		"registry-hub.io/test-id":    environment.RunID,
		"registry-hub.io/instance":   environment.Fullname,
		"registry-hub.io/managed-by": "registry-hub-test-fixture",
	}
}

func applyObject(ctx context.Context, object map[string]any) error {
	content, err := json.Marshal(object)
	if err != nil {
		return err
	}
	_, err = testkit.CommandInput(ctx, content, "kubectl", "apply", "-f", "-")
	return err
}

func patchServiceSelector(ctx context.Context, namespace, name string, selector map[string]any) error {
	patch, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/selector", "value": selector}})
	if err != nil {
		return err
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", namespace, "patch", "service", name, "--type=json", "-p", string(patch))
	return err
}

func waitEndpoint(ctx context.Context, fixture zotFixture, expectedIP string, excludeExpected bool) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		endpoints, err := testkit.KubectlGet(ctx, fixture.namespace, "endpoints", fixture.serviceName)
		if err == nil && endpointMatches(endpoints, expectedIP, excludeExpected) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Registry Service endpoints did not converge")
		case <-ticker.C:
		}
	}
}

func endpointMatches(endpoints map[string]any, expectedIP string, excludeExpected bool) bool {
	for _, rawSubset := range testkit.Slice(endpoints["subsets"]) {
		for _, rawAddress := range testkit.Slice(testkit.Map(rawSubset)["addresses"]) {
			ip := testkit.String(testkit.Map(rawAddress)["ip"])
			if ip == "" {
				continue
			}
			if excludeExpected && ip != expectedIP || !excludeExpected && ip == expectedIP {
				return true
			}
		}
	}
	return false
}
