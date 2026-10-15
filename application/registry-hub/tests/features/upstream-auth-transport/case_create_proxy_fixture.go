package upstreamauthtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"sort"
	"strings"
	"time"
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

const publicAuthorityIP = "34.192.215.158"

func configurePublicAuthorityHosts(ctx context.Context, environment testkit.CaseContext) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	spec := testkit.Map(deployment["spec"])
	template := testkit.Map(spec["template"])
	podSpec := testkit.Map(template["spec"])
	wanted := map[string]struct{}{
		"registry-1.docker.io":            {},
		"auth.docker.io":                  {},
		"github.com":                      {},
		"ghcr.io":                         {},
		"tls.registry-hub.test":           {},
		"tls-untrusted.registry-hub.test": {},
	}
	seen := make(map[string]struct{}, len(wanted))
	aliases := testkit.Slice(podSpec["hostAliases"])
	for _, raw := range aliases {
		alias := testkit.Map(raw)
		for _, rawHost := range testkit.Slice(alias["hostnames"]) {
			host := testkit.String(rawHost)
			if _, ok := wanted[host]; !ok {
				continue
			}
			if testkit.String(alias["ip"]) != publicAuthorityIP {
				return fmt.Errorf("upstream fixture authority %s already has a different host alias", host)
			}
			seen[host] = struct{}{}
		}
	}
	missing := make([]string, 0, len(wanted)-len(seen))
	for host := range wanted {
		if _, ok := seen[host]; !ok {
			missing = append(missing, host)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		aliases = append(aliases, map[string]any{"ip": publicAuthorityIP, "hostnames": missing})
		podSpec["hostAliases"] = aliases
		template["spec"] = podSpec
		spec["template"] = template
		deployment["spec"] = spec
		encoded, err := json.Marshal(deployment)
		if err != nil {
			return err
		}
		if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "replace", "-f", "-"); err != nil {
			return fmt.Errorf("configure public upstream fixture DNS aliases: %w", err)
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
			return fmt.Errorf("backend did not become ready with controlled upstream DNS: %w", err)
		}
	}
	return waitBackendServiceEndpoint(ctx, environment)
}

func waitBackendServiceEndpoint(ctx context.Context, environment testkit.CaseContext) error {
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pods, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "get", "pods", "-l",
			"app.kubernetes.io/instance="+environment.Release+",app.kubernetes.io/component=backend", "-o", "json")
		if err == nil {
			var podList map[string]any
			if json.Unmarshal(pods.Stdout, &podList) == nil {
				readyIPs := make(map[string]struct{})
				for _, rawPod := range testkit.Slice(podList["items"]) {
					pod := testkit.Map(rawPod)
					status := testkit.Map(pod["status"])
					for _, rawCondition := range testkit.Slice(status["conditions"]) {
						condition := testkit.Map(rawCondition)
						if testkit.String(condition["type"]) == "Ready" && testkit.String(condition["status"]) == "True" {
							if ip := testkit.String(status["podIP"]); ip != "" {
								readyIPs[ip] = struct{}{}
							}
						}
					}
				}
				endpoints, endpointErr := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", environment.Fullname)
				if endpointErr == nil {
					for _, rawSubset := range testkit.Slice(endpoints["subsets"]) {
						for _, rawAddress := range testkit.Slice(testkit.Map(rawSubset)["addresses"]) {
							if _, ok := readyIPs[testkit.String(testkit.Map(rawAddress)["ip"])]; ok {
								return nil
							}
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("backend Service endpoint did not converge on a Ready Pod")
		case <-ticker.C:
		}
	}
}

func waitProxyReady(ctx context.Context, environment testkit.CaseContext) error {
	name := environment.Release + proxySuffix
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=180s"); err != nil {
		logs, _ := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+name, "-c", "proxy", "--tail=40")
		return fmt.Errorf("upstream proxy fixture did not become ready: %w: %s", err, strings.TrimSpace(string(logs.Stdout)))
	}
	deadline := time.NewTimer(180 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", name)
		if err == nil {
			for _, subset := range testkit.Slice(endpoints["subsets"]) {
				for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
					if testkit.String(testkit.Map(address)["ip"]) != "" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			logs, _ := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+name, "-c", "proxy", "--tail=40")
			return fmt.Errorf("upstream proxy fixture Service has no ready endpoint: %s", strings.TrimSpace(string(logs.Stdout)))
		case <-ticker.C:
		}
	}
}
