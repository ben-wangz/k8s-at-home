package referrersmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	zotFixtureRepository = "container-registry-mirror.internal.home.geekcity.tech:32443/ghcr.io/project-zot/zot"
	zotFixtureDigest     = "sha256:5d8fb04dd0592f7d7edf42d3cbe925478e75e1cb7b3d9d96f2a596e68f7c1a50"
	zotFixtureImage      = zotFixtureRepository + "@" + zotFixtureDigest
	fixtureLabel         = "registry-hub.io/referrers-migration-fixture"
)

type zotFixture struct {
	namespace        string
	serviceName      string
	deploymentName   string
	podName          string
	configMapName    string
	podIP            string
	originalSelector map[string]any
	registryPodName  string
	replicas         int64
}

type secretVolume struct {
	secretName string
	keys       map[string]string
}

func installZotFixture(ctx context.Context, environment testkit.CaseContext) (fixture zotFixture, resultErr error) {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return zotFixture{}, err
	}
	serviceName := environment.Fullname + "-registry"
	deploymentName := serviceName
	selectorObject := testkit.Map(testkit.Map(deployment["spec"])["selector"])
	selector := testkit.Map(selectorObject["matchLabels"])
	if len(selector) == 0 {
		return zotFixture{}, fmt.Errorf("Registry Deployment has no selector")
	}
	replicas := testkit.Int(testkit.Map(deployment["spec"])["replicas"])
	if replicas != 1 {
		return zotFixture{}, fmt.Errorf("Registry Deployment has %d replicas, expected 1", replicas)
	}
	registryPodName, err := registryPodName(ctx, environment.Namespace, selector)
	if err != nil {
		return zotFixture{}, err
	}
	secrets := registrySecretVolumes(deployment)
	tls := secrets["registry-tls"]
	auth := secrets["registry-auth"]
	if tls.secretName == "" || auth.secretName == "" {
		return zotFixture{}, fmt.Errorf("Registry TLS or htpasswd Secret is missing")
	}
	certKey, keyKey := tls.keys["tls.crt"], tls.keys["tls.key"]
	htpasswdKey := auth.keys["htpasswd"]
	if certKey == "" || keyKey == "" || htpasswdKey == "" {
		return zotFixture{}, fmt.Errorf("Registry TLS or htpasswd Secret key mapping is incomplete")
	}

	fixture = zotFixture{
		namespace: environment.Namespace, serviceName: serviceName,
		deploymentName: deploymentName, registryPodName: registryPodName, replicas: replicas,
		podName:          environment.Release + "-external-referrers-migration-registry",
		configMapName:    environment.Release + "-external-referrers-migration-config",
		originalSelector: selector,
	}
	registryScaledDown := false
	defer func() {
		if resultErr == nil || !registryScaledDown {
			return
		}
		if restoreErr := restoreChartRegistry(ctx, fixture); restoreErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("recover chart Registry after fixture setup failure: %w", restoreErr))
		}
	}()
	labels := make(map[string]any, len(selector)+4)
	for key, value := range selector {
		labels[key] = value
	}
	labels[fixtureLabel] = environment.RunID
	labels["registry-hub.io/test-id"] = environment.RunID
	labels["registry-hub.io/instance"] = environment.Fullname
	labels["registry-hub.io/managed-by"] = "registry-hub-test-fixture"

	config, err := json.Marshal(map[string]any{
		"distSpecVersion": "1.1.0",
		"storage":         map[string]any{"rootDirectory": "/var/lib/zot", "gc": false},
		"http": map[string]any{
			"address": "0.0.0.0", "port": "5000",
			"tls":  map[string]any{"cert": "/etc/zot/tls/tls.crt", "key": "/etc/zot/tls/tls.key"},
			"auth": map[string]any{"htpasswd": map[string]any{"path": "/etc/zot/auth/htpasswd"}},
		},
	})
	if err != nil {
		return zotFixture{}, err
	}
	configMap := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": fixture.configMapName, "namespace": environment.Namespace, "labels": fixtureLabels(environment)},
		"data":     map[string]string{"config.json": string(config)},
	}
	if err := applyObject(ctx, configMap); err != nil {
		return zotFixture{}, fmt.Errorf("create Zot configuration: %w", err)
	}
	pod := zotPod(environment, fixture, labels, tls.secretName, certKey, keyKey, auth.secretName, htpasswdKey)
	if err := applyObject(ctx, pod); err != nil {
		return zotFixture{}, fmt.Errorf("create Zot Registry Pod: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "wait", "--for=condition=Ready", "pod/"+fixture.podName, "--timeout=180s"); err != nil {
		logs, _ := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", fixture.podName, "--tail=80")
		return zotFixture{}, fmt.Errorf("Zot Registry Pod did not become Ready: %w; logs: %s", err, string(logs.Stdout))
	}
	podObject, err := testkit.KubectlGet(ctx, environment.Namespace, "pod", fixture.podName)
	if err != nil {
		return zotFixture{}, err
	}
	fixture.podIP = testkit.String(testkit.Map(podObject["status"])["podIP"])
	if fixture.podIP == "" {
		return zotFixture{}, fmt.Errorf("Zot Registry Pod has no Pod IP")
	}
	registryScaledDown = true
	if err := scaleRegistry(ctx, fixture, 0); err != nil {
		return zotFixture{}, fmt.Errorf("scale down chart Registry before switching Service: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "wait", "--for=delete", "pod/"+fixture.registryPodName, "--timeout=180s"); err != nil {
		return zotFixture{}, fmt.Errorf("wait for chart Registry Pod termination: %w", err)
	}
	if err := patchServiceSelector(ctx, environment.Namespace, serviceName, map[string]any{fixtureLabel: environment.RunID}); err != nil {
		return zotFixture{}, fmt.Errorf("route Registry Service to Zot fixture: %w", err)
	}
	if err := scaleRegistry(ctx, fixture, fixture.replicas); err != nil {
		return zotFixture{}, fmt.Errorf("restore chart Registry Deployment replicas: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+fixture.deploymentName, "--timeout=180s"); err != nil {
		return zotFixture{}, fmt.Errorf("wait for chart Registry Pod after Service switch: %w", err)
	}
	if err := waitEndpoint(ctx, fixture, fixture.podIP, false); err != nil {
		return zotFixture{}, err
	}
	if err := waitBackendReady(ctx, environment); err != nil {
		return fixture, err
	}
	registryScaledDown = false
	return fixture, nil
}

func zotImageReference() string {
	if image := strings.TrimSpace(os.Getenv("REGISTRY_HUB_TEST_ZOT_IMAGE")); image != "" {
		return image
	}
	return zotFixtureImage
}

func waitBackendReady(ctx context.Context, environment testkit.CaseContext) error {
	client := testkit.NewHTTPClient(3*time.Second, nil)
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	endpoint := strings.TrimRight(environment.AdminURL, "/") + "/readyz"
	var lastStatus int
	var lastErr error
	for {
		response, err := testkit.Request(ctx, client, http.MethodGet, endpoint, nil, nil, nil)
		if err == nil {
			lastStatus = response.StatusCode
			response.Body.Close()
			if lastStatus == http.StatusOK {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("backend did not become ready after Registry Service switch (status=%d): %v", lastStatus, lastErr)
		case <-ticker.C:
		}
	}
}

func (fixture zotFixture) Restore(ctx context.Context) error {
	if err := patchServiceSelector(ctx, fixture.namespace, fixture.serviceName, fixture.originalSelector); err != nil {
		return err
	}
	if err := waitEndpoint(ctx, fixture, fixture.podIP, true); err != nil {
		return fmt.Errorf("Registry Service did not return to the chart Deployment: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", fixture.namespace, "delete", "pod", fixture.podName, "--wait=true", "--timeout=60s"); err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", fixture.namespace, "delete", "configmap", fixture.configMapName, "--wait=true", "--timeout=30s"); err != nil && !testkit.IsNotFound(err) {
		return err
	}
	if err := waitEndpoint(ctx, fixture, fixture.podIP, true); err != nil {
		return fmt.Errorf("Registry Service did not return to the chart Deployment: %w", err)
	}
	return nil
}

func registryPodName(ctx context.Context, namespace string, selector map[string]any) (string, error) {
	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+testkit.String(selector[key]))
	}
	output, err := testkit.Command(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", strings.Join(parts, ","), "-o", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output.Stdout, &list); err != nil {
		return "", fmt.Errorf("decode Registry Pod list: %w", err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name == "" {
		return "", fmt.Errorf("Registry Deployment selector matched %d Pods, expected 1", len(list.Items))
	}
	return list.Items[0].Metadata.Name, nil
}

func scaleRegistry(ctx context.Context, fixture zotFixture, replicas int64) error {
	_, err := testkit.Command(ctx, "kubectl", "-n", fixture.namespace, "scale", "deployment/"+fixture.deploymentName, "--replicas="+fmt.Sprint(replicas))
	return err
}
