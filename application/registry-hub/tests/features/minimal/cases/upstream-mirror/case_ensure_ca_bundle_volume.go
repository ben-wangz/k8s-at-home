package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func ensureCABundleVolume(podSpec map[string]any, configMapName string) error {
	volumes := testkit.Slice(podSpec["volumes"])
	for _, raw := range volumes {
		volume := testkit.Map(raw)
		if testkit.String(volume["name"]) != "test-upstream-ca" {
			continue
		}
		configMap := testkit.Map(volume["configMap"])
		if testkit.String(configMap["name"]) != configMapName {
			return fmt.Errorf("test upstream CA volume has an unexpected ConfigMap")
		}
		return nil
	}
	podSpec["volumes"] = append(volumes, map[string]any{
		"name": "test-upstream-ca",
		"configMap": map[string]any{
			"name":  configMapName,
			"items": []map[string]any{{"key": "ca-bundle.crt", "path": "ca-bundle.crt"}},
		},
	})
	return nil
}

func ensureCAMount(mounts []any) []any {
	for _, raw := range mounts {
		mount := testkit.Map(raw)
		if testkit.String(mount["name"]) == "test-upstream-ca" {
			return mounts
		}
	}
	return append(mounts, map[string]any{"name": "test-upstream-ca", "mountPath": "/etc/registry-hub/test-upstream-ca", "readOnly": true})
}

func setEnvValue(values []any, name, value string) []any {
	for index, raw := range values {
		entry := testkit.Map(raw)
		if testkit.String(entry["name"]) != name {
			continue
		}
		entry["value"] = value
		delete(entry, "valueFrom")
		values[index] = entry
		return values
	}
	return append(values, map[string]any{"name": name, "value": value})
}

func upstreamManifestStatus(ctx context.Context, registry *support.RegistryClient, repository, reference string) (int, error) {
	return upstreamManifestStatusWithAccept(ctx, registry, repository, reference, support.OCIManifestMediaType)
}

func upstreamManifestStatusWithAccept(ctx context.Context, registry *support.RegistryClient, repository, reference, accept string) (int, error) {
	response, err := registry.Get(ctx, repository, "/manifests/"+reference, map[string]string{"Accept": accept})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func waitForSolidifiedTask(ctx context.Context, environment testkit.CaseContext, authority, repository, reference string) (map[string]any, error) {
	return waitForTaskStatus(ctx, environment, authority, repository, reference, "solidified")
}

func waitForFailedTask(ctx context.Context, environment testkit.CaseContext, authority, repository, reference string) (map[string]any, error) {
	return waitForTaskStatus(ctx, environment, authority, repository, reference, "failed")
}

func waitForTaskStatus(ctx context.Context, environment testkit.CaseContext, authority, repository, reference, expectedStatus string) (map[string]any, error) {
	return waitForTaskStatusWithin(ctx, environment, authority, repository, reference, expectedStatus, 2*time.Minute)
}

func countTasksForReference(ctx context.Context, environment testkit.CaseContext, authority, repository, reference string) (int, error) {
	items, err := tasksForReference(ctx, environment, authority, repository, reference)
	return len(items), err
}

func tasksForReference(ctx context.Context, environment testkit.CaseContext, authority, repository, reference string) ([]any, error) {
	query := url.Values{
		"authority":  {authority},
		"repository": {repository},
		"digest":     {reference},
		"limit":      {"100"},
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	return testkit.Slice(page["items"]), nil
}

func fixtureMethodsForReference(ctx context.Context, environment testkit.CaseContext, reference string) ([]string, error) {
	marker := "host=github.com path=/v2/registry-hub-test/fixture/manifests/" + reference + " auth=absent"
	return fixtureRequestMethods(ctx, environment, marker, reference)
}

func fixtureMethodsForBlob(ctx context.Context, environment testkit.CaseContext, digest string) ([]string, error) {
	marker := "host=github.com path=/v2/registry-hub-test/fixture/blobs/" + digest + " auth=absent"
	return fixtureRequestMethods(ctx, environment, marker, digest)
}

func fixtureRangeHeadersForBlob(ctx context.Context, environment testkit.CaseContext, digest string) ([]string, error) {
	marker := "host=github.com path=/v2/registry-hub-test/fixture/blobs/" + digest + " auth=absent"
	logs, err := fixtureLogs(ctx, environment)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, 1)
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		rangeStart := strings.Index(line, " range=")
		acceptStart := strings.Index(line, " accept=")
		if rangeStart < 0 || acceptStart < rangeStart {
			return nil, fmt.Errorf("upstream fixture log has no Range value for %q", digest)
		}
		quoted := strings.TrimSpace(line[rangeStart+len(" range=") : acceptStart])
		value, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, fmt.Errorf("upstream fixture logged an invalid Range value for %q", digest)
		}
		values = append(values, value)
	}
	return values, nil
}

func fixtureRequestMethods(ctx context.Context, environment testkit.CaseContext, marker, reference string) ([]string, error) {
	logs, err := fixtureLogs(ctx, environment)
	if err != nil {
		return nil, err
	}
	methods := make([]string, 0, 1)
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		switch {
		case strings.Contains(line, "method="+http.MethodHead+" "):
			methods = append(methods, http.MethodHead)
		case strings.Contains(line, "method="+http.MethodGet+" "):
			methods = append(methods, http.MethodGet)
		default:
			return nil, fmt.Errorf("upstream fixture log has no recognized method for %q", reference)
		}
	}
	return methods, nil
}

func fixtureLogs(ctx context.Context, environment testkit.CaseContext) (string, error) {
	name := environment.Release + "-external-upstream-proxy"
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+name, "-c", "proxy", "--tail=500")
	if err != nil {
		return "", err
	}
	return string(result.Stdout), nil
}

type registryAccessEvent struct {
	method string
	path   string
	query  url.Values
	status int
}

func registryLogLines(ctx context.Context, environment testkit.CaseContext, since time.Time) ([]string, error) {
	name := environment.Fullname + "-registry"
	sinceText := since.UTC().Format(time.RFC3339Nano)
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+name, "-c", "registry", "--since-time="+sinceText, "--tail=20000")
	if err != nil {
		return nil, fmt.Errorf("read Distribution access logs: %w", err)
	}
	return strings.Split(strings.TrimSpace(string(result.Stdout)), "\n"), nil
}
