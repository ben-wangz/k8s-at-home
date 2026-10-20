package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func (e *Environment) Deploy(ctx context.Context, directory string, record *Record) error {
	parts := strings.SplitN(record.MihomoImage, "@", 2)
	if len(parts) != 2 {
		return fmt.Errorf("immutable Mihomo image is required")
	}
	labels := map[string]string{RunLabel: record.ID}
	values := map[string]any{
		"fullnameOverride": record.Release, "commonLabels": labels, "podLabels": labels,
		"subscription": map[string]any{"existingSecret": record.Release + "-subscription"},
		"image":        map[string]string{"repository": parts[0], "digest": parts[1]},
		"service":      map[string]string{"type": "ClusterIP"},
		"provider":     map[string]any{"healthCheck": map[string]bool{"enabled": false}},
	}
	if record.Scenario == "feature-controller-auth" || record.Scenario == "feature-ingress" {
		values["controller"] = map[string]string{"existingSecret": record.Release + "-controller"}
	}
	if record.Scenario == "feature-ingress" {
		class := os.Getenv("CLASH_TEST_INGRESS_CLASS")
		if class == "" {
			class = "nginx"
		}
		values["ingress"] = map[string]any{"controller": map[string]any{"enabled": true, "hostname": os.Getenv("CLASH_TEST_INGRESS_HOST"), "tls": true, "secretName": record.Release + "-tls", "ingressClassName": class}}
	}
	path := filepath.Join(directory, "environment-values.json")
	if err := testkit.WriteJSON(path, values); err != nil {
		return err
	}
	scenario := filepath.Join(e.App, "tests/features", strings.TrimPrefix(record.Scenario, "feature-"), "config.yaml")
	output, err := e.Helm(ctx, "upgrade", "--install", record.Release, filepath.Join(e.App, "chart"), "-f", scenario, "-f", path, "--wait", "--timeout", "240s")
	_ = os.WriteFile(filepath.Join(directory, "helm.log"), output, 0600)
	if err != nil {
		return fmt.Errorf("chart deployment failed; see helm.log: %w", err)
	}
	if err := e.Collect(ctx, record); err != nil {
		return err
	}
	output, err = e.Kubectl(ctx, nil, "get", "service", record.Release, "-o", "json")
	if err != nil {
		return err
	}
	var service struct {
		Spec struct {
			IP    string `json:"clusterIP"`
			Ports []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(output, &service); err != nil {
		return err
	}
	for _, port := range service.Spec.Ports {
		address := fmt.Sprintf("http://%s:%d", service.Spec.IP, port.Port)
		if port.Name == "controller" {
			record.ControllerURL = address
		}
		if port.Name == "http" {
			record.ProxyURL = address
		}
	}
	output, err = e.Kubectl(ctx, nil, "get", "service", record.Release+"-fixture", "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return err
	}
	record.TrafficURL = "http://" + record.Release + "-fixture." + e.Namespace + ".svc.cluster.local:8080/stream"
	record.FrontendURL = record.ControllerURL + "/ui/"
	if record.Scenario == "feature-ingress" {
		record.ControllerURL = "https://" + os.Getenv("CLASH_TEST_INGRESS_HOST")
		record.FrontendURL = record.ControllerURL + "/ui/"
	}
	secret, _ := os.ReadFile(filepath.Join(directory, "controller-secret"))
	request, _ := http.NewRequestWithContext(ctx, "GET", record.ControllerURL+"/version", nil)
	if len(secret) > 0 {
		request.Header.Set("Authorization", "Bearer "+string(secret))
	}
	client := &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("controller is not directly reachable from the test workstation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("controller preflight returned HTTP %d", response.StatusCode)
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		return err
	}
	record.MihomoVersion = version.Version
	record.State = "environment-ready"
	return Save(directory, record)
}
