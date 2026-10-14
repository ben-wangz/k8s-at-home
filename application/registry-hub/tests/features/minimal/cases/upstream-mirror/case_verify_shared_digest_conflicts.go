package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"strings"
)

func verifySharedDigestConflicts(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	conflicts := []struct {
		reference string
		kind      string
	}{
		{reference: "minimal-shared-size-conflict", kind: "size"},
		{reference: "minimal-shared-media-conflict", kind: "mediaType"},
	}
	result := make(map[string]any, len(conflicts)*4)
	for _, conflict := range conflicts {
		body, headers, err := support.ReadManifest(ctx, registry, repository, conflict.reference, support.OCIIndexMediaType)
		if err != nil {
			return nil, fmt.Errorf("read %s fixture: %w", conflict.kind, err)
		}
		digest := support.Digest(body)
		if headers.Get("Docker-Content-Digest") != digest {
			return nil, fmt.Errorf("%s conflict fixture root digest is invalid", conflict.kind)
		}
		var index support.Manifest
		if err := json.Unmarshal(body, &index); err != nil || len(index.Manifests) != 2 || index.Manifests[0].Digest != index.Manifests[1].Digest {
			return nil, fmt.Errorf("%s conflict fixture does not contain one shared digest edge", conflict.kind)
		}
		switch conflict.kind {
		case "size":
			if index.Manifests[1].Size != index.Manifests[0].Size+1 {
				return nil, fmt.Errorf("shared-size conflict fixture has no size mismatch")
			}
		case "mediaType":
			if index.Manifests[0].MediaType == index.Manifests[1].MediaType {
				return nil, fmt.Errorf("shared-media conflict fixture has no media type mismatch")
			}
		}
		task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, conflict.reference)
		if err != nil {
			return nil, err
		}
		if testkit.String(task["errorClass"]) != "manifest_invalid" || testkit.String(task["rootDigest"]) != digest || task["rootPublished"] == true {
			return nil, fmt.Errorf("%s conflict task has invalid terminal state: class=%q root=%q published=%v", conflict.kind, testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"])
		}
		taskCount, err := countTasksForReference(ctx, environment, "github.com", upstreamRepository, conflict.reference)
		if err != nil {
			return nil, err
		}
		if taskCount != 1 {
			return nil, fmt.Errorf("%s conflict produced %d tasks, want one", conflict.kind, taskCount)
		}
		prefix := "shared" + strings.ToUpper(conflict.kind[:1]) + conflict.kind[1:]
		result[prefix+"ConflictTaskStatus"] = testkit.String(task["status"])
		result[prefix+"ConflictErrorClass"] = testkit.String(task["errorClass"])
		result[prefix+"ConflictRootDigest"] = testkit.String(task["rootDigest"])
		result[prefix+"ConflictRootUnpublished"] = task["rootPublished"] != true
	}
	return result, nil
}

func verifyPartialUpstreamBlob(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	partialBlob := []byte("partial-upstream-range-fixture")
	partialDigest := support.Digest(partialBlob)
	manifestBody, _, err := support.ReadManifest(ctx, registry, repository, "minimal-partial-blob", support.OCIManifestMediaType)
	if err != nil {
		return nil, err
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil || len(manifest.Layers) != 1 || manifest.Layers[0].Digest != partialDigest || manifest.Layers[0].Size != len(partialBlob) {
		return nil, fmt.Errorf("partial response fixture manifest has an invalid layer descriptor")
	}
	task, err := waitForTaskStatus(ctx, environment, "github.com", upstreamRepository, "minimal-partial-blob", "retry_wait")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["errorClass"]) != "upstream" || task["retryable"] != true || testkit.String(task["rootDigest"]) == "" || task["rootPublished"] == true {
		return nil, fmt.Errorf("partial upstream response task has invalid retry state: class=%q retryable=%v root=%q published=%v", testkit.String(task["errorClass"]), task["retryable"], testkit.String(task["rootDigest"]), task["rootPublished"])
	}
	if testkit.Map(task["checkpoints"])[partialDigest] != nil {
		return nil, fmt.Errorf("partial upstream blob received a verified checkpoint")
	}
	partialResponse, err := registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/blobs/"+partialDigest), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("read blob after partial upstream response: %w", err)
	}
	partialResponseBody, readErr := io.ReadAll(io.LimitReader(partialResponse.Body, 1<<20))
	_ = partialResponse.Body.Close()
	if readErr != nil || partialResponse.StatusCode != http.StatusBadGateway || partialResponse.Header.Get("Content-Range") != "" || string(partialResponseBody) == string(partialBlob[:3]) {
		return nil, fmt.Errorf("partial upstream bytes were exposed: status=%d contentRange=%q bodyBytes=%d", partialResponse.StatusCode, partialResponse.Header.Get("Content-Range"), len(partialResponseBody))
	}
	logs, err := fixtureLogs(ctx, environment)
	if err != nil {
		return nil, err
	}
	marker := "path=/v2/registry-hub-test/fixture/blobs/" + partialDigest + " auth=absent"
	requestCount := 0
	allRequestsWithoutRange := true
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		requestCount++
		if !strings.Contains(line, `range=""`) {
			allRequestsWithoutRange = false
		}
	}
	if requestCount == 0 || !allRequestsWithoutRange {
		return nil, fmt.Errorf("partial upstream blob requests=%d allWithoutRange=%v", requestCount, allRequestsWithoutRange)
	}
	updatedTask, err := waitForTaskStatus(ctx, environment, "github.com", upstreamRepository, "minimal-partial-blob", "retry_wait")
	if err != nil {
		return nil, err
	}
	if testkit.Map(updatedTask["checkpoints"])[partialDigest] != nil {
		return nil, fmt.Errorf("partial upstream blob received a checkpoint after the client retry")
	}
	return map[string]any{
		"partialBlobDigest":                         partialDigest,
		"partialBlobTaskStatus":                     testkit.String(task["status"]),
		"partialBlobErrorClass":                     testkit.String(task["errorClass"]),
		"partialBlobRetryable":                      task["retryable"],
		"partialBlobRootUnpublished":                task["rootPublished"] != true,
		"partialBlobCheckpoint":                     testkit.Map(task["checkpoints"])[partialDigest],
		"partialBlobUpstreamRequests":               requestCount,
		"partialBlobRangeAbsent":                    allRequestsWithoutRange,
		"partialBlobClientStatus":                   partialResponse.StatusCode,
		"partialBlobClientContentRange":             partialResponse.Header.Get("Content-Range"),
		"partialBlobClientDidNotReceivePartialBody": string(partialResponseBody) != string(partialBlob[:3]),
		"partialBlobCheckpointAfterClientRetry":     testkit.Map(updatedTask["checkpoints"])[partialDigest],
	}, nil
}

func expectedNestedDiscoveryFixture() (nestedExpectedObject, map[string]nestedExpectedObject, []byte, error) {
	config := []byte("nested-platform-config")
	configDigest := support.Digest(config)
	platforms := []struct {
		name         string
		os           string
		architecture string
		variant      string
		osVersion    string
	}{
		{name: "linux-amd64", os: "linux", architecture: "amd64", variant: "v3"},
		{name: "linux-arm64", os: "linux", architecture: "arm64", variant: "v8"},
		{name: "windows-amd64", os: "windows", architecture: "amd64", osVersion: "10.0.20348.0"},
		{name: "freebsd-riscv64", os: "freebsd", architecture: "riscv64"},
	}
	objects := make(map[string]nestedExpectedObject, len(platforms)+2)
	linuxDescriptors := make([]any, 0, 2)
	rootDescriptors := make([]any, 0, 2)
	for _, platform := range platforms {
		manifestBody, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     support.OCIManifestMediaType,
			"annotations":   map[string]string{"org.registry-hub.fixture.platform": platform.name},
			"config": map[string]any{
				"mediaType": "application/vnd.oci.image.config.v1+json",
				"size":      len(config),
				"digest":    configDigest,
			},
			"layers": []any{},
		})
		if err != nil {
			return nestedExpectedObject{}, nil, nil, err
		}
		manifestDigest := support.Digest(manifestBody)
		platformFields := map[string]string{"os": platform.os, "architecture": platform.architecture}
		if platform.variant != "" {
			platformFields["variant"] = platform.variant
		}
		if platform.osVersion != "" {
			platformFields["os.version"] = platform.osVersion
		}
		objects[manifestDigest] = nestedExpectedObject{body: manifestBody, mediaType: support.OCIManifestMediaType, platform: platformFields}
		descriptor := map[string]any{
			"mediaType": support.OCIManifestMediaType,
			"size":      len(manifestBody),
			"digest":    manifestDigest,
			"platform":  platformFields,
			"annotations": map[string]string{
				"org.registry-hub.fixture.platform": platform.name,
			},
		}
		if platform.os == "linux" {
			linuxDescriptors = append(linuxDescriptors, descriptor)
		} else {
			rootDescriptors = append(rootDescriptors, descriptor)
		}
	}
	leafBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.group": "linux-leaf"},
		"manifests":     linuxDescriptors,
	})
	if err != nil {
		return nestedExpectedObject{}, nil, nil, err
	}
	leafDigest := support.Digest(leafBody)
	objects[leafDigest] = nestedExpectedObject{body: leafBody, mediaType: support.OCIIndexMediaType}
	nestedBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.group": "linux"},
		"manifests": []any{map[string]any{
			"mediaType": support.OCIIndexMediaType,
			"size":      len(leafBody),
			"digest":    leafDigest,
		}},
	})
	if err != nil {
		return nestedExpectedObject{}, nil, nil, err
	}
	nestedDigest := support.Digest(nestedBody)
	objects[nestedDigest] = nestedExpectedObject{body: nestedBody, mediaType: support.OCIIndexMediaType}
	rootDescriptors = append([]any{map[string]any{
		"mediaType":   support.OCIIndexMediaType,
		"size":        len(nestedBody),
		"digest":      nestedDigest,
		"urls":        []string{"https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch"},
		"annotations": map[string]string{"org.registry-hub.fixture.group": "linux"},
	}}, rootDescriptors...)
	rootBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.root": "all-platforms"},
		"manifests":     rootDescriptors,
	})
	if err != nil {
		return nestedExpectedObject{}, nil, nil, err
	}
	return nestedExpectedObject{body: rootBody, mediaType: support.OCIIndexMediaType}, objects, config, nil
}
