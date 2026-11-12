package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func verifyPublishTopology(lines []string, childDigest, indexDigest, configDigest string) (map[string]any, error) {
	const repositoryPath = "/v2/egithub_2ecom/registry-hub-test/fixture"
	events := make([]registryAccessEvent, 0, len(lines))
	for _, line := range lines {
		event, ok := parseRegistryAccessEvent(line)
		if !ok || !strings.HasPrefix(event.path, repositoryPath+"/") {
			continue
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("Distribution access log delta contains no requests for %s", repositoryPath)
	}

	blobPath := repositoryPath + "/blobs/" + configDigest
	childPath := repositoryPath + "/manifests/" + childDigest
	indexPath := repositoryPath + "/manifests/" + indexDigest
	tagPath := repositoryPath + "/manifests/minimal-discovery"
	blobMiss := findRegistryEvent(events, registryRequest(http.MethodHead, blobPath, http.StatusNotFound), 0, len(events))
	blobPut := findRegistryEvent(events, func(event registryAccessEvent) bool {
		return event.method == http.MethodPut && event.status == http.StatusCreated && strings.HasPrefix(event.path, repositoryPath+"/blobs/uploads/") && event.query.Get("digest") == configDigest
	}, 0, len(events))
	if blobPut < 0 {
		var uploads []string
		for _, event := range events {
			if event.method == http.MethodPut && strings.Contains(event.path, "/blobs/uploads/") {
				uploads = append(uploads, fmt.Sprintf("%s status=%d digest=%s", event.path, event.status, event.query.Get("digest")))
			}
		}
		return nil, fmt.Errorf("Distribution trace has no verified config blob upload for %s; observed uploads: %v", configDigest, uploads)
	}
	blobConfirmed := findRegistryEvent(events, registryRequest(http.MethodHead, blobPath, http.StatusOK), blobPut+1, len(events))
	childPut := findRegistryEvent(events, registryRequest(http.MethodPut, childPath, http.StatusCreated), blobPut+1, len(events))
	childMiss := findLastRegistryEvent(events, registryRequest(http.MethodGet, childPath, http.StatusNotFound), blobConfirmed+1, childPut)
	indexPut := findRegistryEvent(events, registryRequest(http.MethodPut, indexPath, http.StatusCreated), childPut+1, len(events))
	indexMiss := findLastRegistryEvent(events, registryRequest(http.MethodGet, indexPath, http.StatusNotFound), childPut+1, indexPut)
	tagMiss := findLastRegistryEvent(events, registryRequest(http.MethodHead, tagPath, http.StatusNotFound), indexPut+1, len(events))
	tagPut := findRegistryEvent(events, registryRequest(http.MethodPut, tagPath, http.StatusCreated), tagMiss+1, len(events))
	steps := []struct {
		name  string
		index int
	}{
		{name: "config blob HEAD miss", index: blobMiss},
		{name: "config blob PUT", index: blobPut},
		{name: "config blob HEAD confirmation", index: blobConfirmed},
		{name: "child manifest miss", index: childMiss},
		{name: "child manifest PUT", index: childPut},
		{name: "index manifest miss", index: indexMiss},
		{name: "index manifest PUT", index: indexPut},
		{name: "root tag HEAD miss", index: tagMiss},
		{name: "root tag PUT", index: tagPut},
	}
	sequence := make([]int, 0, len(steps))
	for _, step := range steps {
		if step.index < 0 {
			return nil, fmt.Errorf("Distribution trace is missing %s", step.name)
		}
		sequence = append(sequence, step.index)
	}
	for index := 1; index < len(sequence); index++ {
		if sequence[index] <= sequence[index-1] {
			return nil, fmt.Errorf("Distribution trace violates write-time integrity and topology publication order")
		}
	}
	if findRegistryEvent(events, registryRequest(http.MethodGet, blobPath, http.StatusOK), blobPut+1, len(events)) >= 0 {
		return nil, fmt.Errorf("Distribution trace contains a successful blob payload GET readback after publication")
	}
	return map[string]any{
		"publishTopologyOrdered": true,
		"publishTopologySequence": []string{
			"config blob HEAD 404", "config blob PUT 201", "config blob HEAD 200",
			"child manifest GET 404", "child manifest PUT 201",
			"root index GET 404", "root index PUT 201",
			"root tag HEAD 404", "root tag PUT 201",
			"no successful blob payload GET readbacks",
		},
	}, nil
}

func parseRegistryAccessEvent(line string) (registryAccessEvent, bool) {
	method, methodOK := registryLogField(line, "http.request.method")
	requestURI, uriOK := registryLogField(line, "http.request.uri")
	statusText, statusOK := registryLogField(line, "http.response.status")
	if !methodOK || !uriOK || !statusOK {
		return registryAccessEvent{}, false
	}
	parsedURI, err := url.ParseRequestURI(requestURI)
	status, statusErr := strconv.Atoi(statusText)
	if err != nil || statusErr != nil {
		return registryAccessEvent{}, false
	}
	return registryAccessEvent{method: method, path: parsedURI.EscapedPath(), query: parsedURI.Query(), status: status}, true
}

func registryRequest(method, path string, status int) func(registryAccessEvent) bool {
	return func(event registryAccessEvent) bool {
		return event.method == method && event.path == path && event.status == status
	}
}

func findRegistryEvent(events []registryAccessEvent, match func(registryAccessEvent) bool, start, end int) int {
	if start < 0 {
		start = 0
	}
	if end > len(events) {
		end = len(events)
	}
	for index := start; index < end; index++ {
		if match(events[index]) {
			return index
		}
	}
	return -1
}

func findLastRegistryEvent(events []registryAccessEvent, match func(registryAccessEvent) bool, start, end int) int {
	if end > len(events) {
		end = len(events)
	}
	for index := end - 1; index >= start; index-- {
		if match(events[index]) {
			return index
		}
	}
	return -1
}

func registryLogField(line, field string) (string, bool) {
	marker := field + "="
	start := strings.Index(line, marker)
	if start < 0 {
		return "", false
	}
	value := line[start+len(marker):]
	if strings.HasPrefix(value, "\"") {
		for index := 1; index < len(value); index++ {
			if value[index] == '\\' {
				index++
				continue
			}
			if value[index] == '"' {
				decoded, err := strconv.Unquote(value[:index+1])
				return decoded, err == nil
			}
		}
		return "", false
	}
	if end := strings.IndexAny(value, " \t"); end >= 0 {
		value = value[:end]
	}
	return value, value != ""
}

func countFixtureRequests(ctx context.Context, environment testkit.CaseContext, reference string) (int, error) {
	return countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/manifests/"+reference)
}

func countFixturePath(ctx context.Context, environment testkit.CaseContext, path string) (int, error) {
	return countFixturePathForHost(ctx, environment, "github.com", path)
}

func countFixturePathForHost(ctx context.Context, environment testkit.CaseContext, host, path string) (int, error) {
	logs, err := fixtureLogs(ctx, environment)
	if err != nil {
		return 0, err
	}
	marker := "host=" + host + " path=" + path + " auth=absent"
	return strings.Count(logs, marker), nil
}

func fixtureAcceptsForReference(ctx context.Context, environment testkit.CaseContext, reference string) ([]string, error) {
	logs, err := fixtureLogs(ctx, environment)
	if err != nil {
		return nil, err
	}
	marker := "host=github.com path=/v2/registry-hub-test/fixture/manifests/" + reference + " auth=absent"
	accepts := make([]string, 0, 2)
	for _, line := range strings.Split(logs, "\n") {
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		afterMarker := line[index+len(marker):]
		acceptIndex := strings.Index(afterMarker, " accept=")
		if acceptIndex < 0 {
			return nil, fmt.Errorf("upstream fixture log has no Accept value")
		}
		value, err := strconv.Unquote(strings.TrimSpace(afterMarker[acceptIndex+len(" accept="):]))
		if err != nil {
			return nil, fmt.Errorf("upstream fixture logged an invalid Accept value")
		}
		accepts = append(accepts, value)
	}
	return accepts, nil
}

const discoveryConfigBody = "registry-hub-discovery-config"

func discoveryIndexFixture() ([]byte, []byte, error) {
	config := []byte(discoveryConfigBody)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIManifestMediaType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(config),
			"digest":    support.Digest(config),
		},
		"layers": []any{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal discovery child manifest: %w", err)
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIIndexMediaType,
		"manifests": []any{map[string]any{
			"mediaType": support.OCIManifestMediaType,
			"digest":    support.Digest(manifest),
			"size":      len(manifest),
			"platform": map[string]any{
				"architecture": "amd64",
				"os":           "linux",
				"variant":      "v3",
				"os.version":   "fixture-1",
				"os.features":  []string{"fixture-feature"},
			},
			"annotations":          map[string]string{"org.opencontainers.image.ref.name": "fixture-child"},
			"x-fixture-descriptor": "retained",
		}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal discovery OCI index: %w", err)
	}
	return manifest, index, nil
}

func fixtureLabels(environment testkit.CaseContext, fixtureName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance": environment.Release,
		"registry-hub.io/test-id":    environment.RunID,
		"registry-hub.io/instance":   environment.Fullname,
		"registry-hub.io/managed-by": "registry-hub-test-fixture",
		"registry-hub.io/fixture":    fixtureName,
	}
}
