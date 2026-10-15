package upstreamauthtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

func CreateProxyFixture(ctx context.Context, environment testkit.CaseContext, secretName string, fixtureSources, extraConfig map[string]string) error {
	fixtureName := environment.Release + proxySuffix
	labels := map[string]string{
		"app.kubernetes.io/instance": environment.Release,
		"registry-hub.io/test-id":    environment.RunID,
		"registry-hub.io/instance":   environment.Fullname,
		"registry-hub.io/managed-by": "registry-hub-test-fixture",
		"registry-hub.io/fixture":    fixtureName,
	}
	configData := make(map[string]string, len(fixtureSources)+len(extraConfig))
	for name, source := range fixtureSources {
		if !strings.HasSuffix(name, ".go") || strings.Contains(name, "/") || name == "" {
			return fmt.Errorf("upstream fixture source name %q is invalid", name)
		}
		configData[name] = source
	}
	for key, value := range extraConfig {
		if key == "" || strings.HasSuffix(key, ".go") || strings.Contains(key, "/") {
			return fmt.Errorf("upstream fixture ConfigMap key %q is invalid", key)
		}
		configData[key] = value
	}
	resources := []map[string]any{
		{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": fixtureName, "namespace": environment.Namespace, "labels": labels},
			"data":     configData,
		},
		{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": fixtureName, "namespace": environment.Namespace, "labels": labels},
			"spec": map[string]any{
				"selector": map[string]string{"registry-hub.io/fixture": fixtureName},
				"ports":    []map[string]any{{"name": "http", "port": 8080, "targetPort": 8080}},
			},
		},
		{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": fixtureName, "namespace": environment.Namespace, "labels": labels},
			"spec": map[string]any{
				"automountServiceAccountToken": false,
				"restartPolicy":                "Always",
				"securityContext": map[string]any{
					"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
					"fsGroup":        65532,
					"seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
				"containers": []map[string]any{{
					"name": "proxy", "image": proxyImage, "imagePullPolicy": "IfNotPresent",
					"command": []string{"/bin/sh", "-c"},
					"args":    []string{"CGO_ENABLED=0 GOCACHE=/tmp/gocache GO111MODULE=off go run /fixture/*.go"},
					"envFrom": []map[string]any{{"secretRef": map[string]any{"name": secretName}}},
					"ports":   []map[string]any{{"name": "http", "containerPort": 8080}},
					"readinessProbe": map[string]any{
						"httpGet": map[string]any{"path": "/healthz", "port": "http"}, "periodSeconds": 2, "failureThreshold": 60,
					},
					"resources": map[string]any{
						"requests": map[string]string{"cpu": "100m", "memory": "128Mi"},
						"limits":   map[string]string{"cpu": "1", "memory": "512Mi"},
					},
					"securityContext": map[string]any{
						"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
						"capabilities": map[string]any{"drop": []string{"ALL"}},
					},
					"volumeMounts": []map[string]any{
						{"name": "source", "mountPath": "/fixture", "readOnly": true},
						{"name": "upstream-tls", "mountPath": "/upstream-tls", "readOnly": true},
						{"name": "tmp", "mountPath": "/tmp"},
					},
				}},
				"volumes": []map[string]any{
					{"name": "source", "configMap": map[string]any{"name": fixtureName}},
					{"name": "upstream-tls", "secret": map[string]any{
						"secretName": secretName,
						"items": []map[string]any{
							{"key": "UPSTREAM_TLS_CERT", "path": "tls.crt"},
							{"key": "UPSTREAM_TLS_KEY", "path": "tls.key"},
						},
					}},
					{"name": "tmp", "emptyDir": map[string]any{}},
				},
			},
		},
	}
	for _, resource := range resources {
		encoded, err := json.Marshal(resource)
		if err != nil {
			return err
		}
		if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "apply", "-f", "-"); err != nil {
			return fmt.Errorf("create upstream proxy fixture resource: %w", err)
		}
	}
	return nil
}

func WaitProxyReady(ctx context.Context, environment testkit.CaseContext) error {
	return waitProxyReady(ctx, environment)
}

func WaitBackendServiceEndpoint(ctx context.Context, environment testkit.CaseContext) error {
	return waitBackendServiceEndpoint(ctx, environment)
}
