package backends3

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func readS3AttemptMetrics(ctx context.Context, environment testkit.CaseContext) (map[string]float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("metrics returned %s", response.Status)
	}
	body, err := testkit.ReadBody(response, 8<<20)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"domain": "state"}
	values := make(map[string]float64, 2)
	for _, name := range []string{"registry_hub_s3_http_attempts_total", "registry_hub_s3_http_attempt_duration_seconds_count"} {
		value, found, err := testkit.PrometheusSum(body, name, labels)
		if err != nil {
			return nil, err
		}
		if !found || value <= 0 {
			return nil, fmt.Errorf("backend S3 metric %s has no positive state-domain samples", name)
		}
		values[name] = value
	}
	return values, nil
}

func verifyBackendS3Environment(ctx context.Context, environment testkit.CaseContext, secretName string) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	template := testkit.Map(testkit.Map(deployment["spec"])["template"])
	podSpec := testkit.Map(template["spec"])
	var backend map[string]any
	for _, raw := range testkit.Slice(podSpec["containers"]) {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == "backend" {
			backend = container
			break
		}
	}
	if backend == nil {
		return fmt.Errorf("backend Deployment has no backend container")
	}
	fixtureName := environment.Release + backendS3SecretSuffix
	expected := map[string]string{
		"REGISTRY_HUB_STORAGE_BACKEND":        "s3",
		"REGISTRY_HUB_S3_ENDPOINT":            fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", fixtureName, environment.Namespace),
		"REGISTRY_HUB_S3_REGION":              "us-east-1",
		"REGISTRY_HUB_S3_BUCKET":              "rh-" + environment.RunID,
		"REGISTRY_HUB_S3_PREFIX":              backendS3Prefix,
		"REGISTRY_HUB_S3_FORCE_PATH_STYLE":    "true",
		"REGISTRY_HUB_S3_ALLOW_HTTP":          "true",
		"REGISTRY_HUB_S3_MULTIPART_THRESHOLD": "5242880",
		"REGISTRY_HUB_S3_PART_SIZE":           "5242880",
	}
	actual := make(map[string]string, len(expected))
	for _, raw := range testkit.Slice(backend["env"]) {
		item := testkit.Map(raw)
		name := testkit.String(item["name"])
		if _, wanted := expected[name]; wanted {
			actual[name] = testkit.String(item["value"])
		}
	}
	for name, wanted := range expected {
		if actual[name] != wanted {
			return fmt.Errorf("backend S3 environment %s is %q, want %q", name, actual[name], wanted)
		}
	}
	for _, raw := range testkit.Slice(backend["envFrom"]) {
		secretRef := testkit.Map(testkit.Map(raw)["secretRef"])
		if testkit.String(secretRef["name"]) == secretName {
			return nil
		}
	}
	return fmt.Errorf("backend Deployment does not load the S3 credentials Secret")
}
