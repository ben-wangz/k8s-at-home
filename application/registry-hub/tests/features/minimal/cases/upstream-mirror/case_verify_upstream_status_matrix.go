package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

func verifyUpstreamStatusMatrix(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	tests := []struct {
		reference    string
		status       int
		code         string
		requestCount int
	}{
		{reference: "minimal-upstream-401", status: http.StatusUnauthorized, code: "UNAUTHORIZED", requestCount: 1},
		{reference: "minimal-upstream-403", status: http.StatusForbidden, code: "DENIED", requestCount: 1},
		{reference: "minimal-upstream-408", status: http.StatusBadGateway, code: "UNKNOWN", requestCount: 3},
		{reference: "minimal-upstream-429", status: http.StatusBadGateway, code: "UNKNOWN", requestCount: 3},
		{reference: "minimal-upstream-500", status: http.StatusInternalServerError, code: "UNAVAILABLE", requestCount: 1},
		{reference: "minimal-upstream-reset", status: http.StatusBadGateway, code: "UNKNOWN", requestCount: 3},
		{reference: "minimal-upstream-502", status: http.StatusBadGateway, code: "UNKNOWN", requestCount: 3},
		{reference: "minimal-upstream-504", status: http.StatusBadGateway, code: "UNKNOWN", requestCount: 3},
	}
	statuses := make([]int, 0, len(tests))
	requestCounts := make([]int, 0, len(tests))
	codes := make([]string, 0, len(tests))
	for _, test := range tests {
		response, err := registry.Get(ctx, repository, "/manifests/"+test.reference, map[string]string{"Accept": support.OCIManifestMediaType})
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("read upstream %s error response: %v %v", test.reference, readErr, closeErr)
		}
		if response.StatusCode != test.status || response.Header.Get("WWW-Authenticate") != "" || strings.Contains(string(body), "upstream-fixture-secret") {
			return nil, fmt.Errorf("upstream %s returned status=%d challenge=%q body=%q", test.reference, response.StatusCode, response.Header.Get("WWW-Authenticate"), body)
		}
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("upstream %s error body is not JSON: %w", test.reference, err)
		}
		errors := testkit.Slice(envelope["errors"])
		if len(errors) != 1 {
			return nil, fmt.Errorf("upstream %s error envelope entries = %d, want 1", test.reference, len(errors))
		}
		code := testkit.String(testkit.Map(errors[0])["code"])
		if code != test.code {
			return nil, fmt.Errorf("upstream %s error envelope code = %q, want %q", test.reference, code, test.code)
		}
		task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, test.reference)
		if err != nil {
			return nil, err
		}
		if testkit.String(task["phase"]) != "discovery" || testkit.String(task["errorClass"]) != "upstream" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true {
			return nil, fmt.Errorf("upstream %s failure task advanced beyond discovery: phase=%q class=%q root=%q published=%v", test.reference, testkit.String(task["phase"]), testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"])
		}
		requests, err := countFixtureRequests(ctx, environment, test.reference)
		if err != nil {
			return nil, err
		}
		if requests != test.requestCount {
			return nil, fmt.Errorf("upstream %s requests = %d, want %d", test.reference, requests, test.requestCount)
		}
		statuses = append(statuses, response.StatusCode)
		requestCounts = append(requestCounts, requests)
		codes = append(codes, code)
	}
	return map[string]any{
		"upstreamStatusMatrixReferences": []string{"minimal-upstream-401", "minimal-upstream-403", "minimal-upstream-408", "minimal-upstream-429", "minimal-upstream-500", "minimal-upstream-reset", "minimal-upstream-502", "minimal-upstream-504"},
		"upstreamStatusMatrixStatuses":   statuses,
		"upstreamStatusMatrixCodes":      codes,
		"upstreamStatusMatrixRequests":   requestCounts,
		"upstreamAuthChallengeRedacted":  true,
		"upstreamStatusErrorsAreNotMiss": true,
	}, nil
}

func verifyUpstreamRetryAfter(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	const reference = "minimal-retry-after"
	startedAt := time.Now()
	body, headers, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("Retry-After upstream response did not recover: %w", err)
	}
	elapsed := time.Since(startedAt)
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || mediaType != support.OCIManifestMediaType || headers.Get("Docker-Content-Digest") != support.Digest(body) {
		return nil, fmt.Errorf("Retry-After recovery returned invalid manifest metadata")
	}
	if elapsed < 9*time.Second {
		return nil, fmt.Errorf("Retry-After total wait took %s, want at least 9s", elapsed)
	}
	requests, err := countFixtureRequests(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	if requests != 3 {
		return nil, fmt.Errorf("Retry-After fixture requests = %d, want 3", requests)
	}
	task, err := waitForSolidifiedTask(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, err
	}
	if testkit.String(task["rootDigest"]) != support.Digest(body) || task["rootPublished"] != true {
		return nil, fmt.Errorf("Retry-After recovery did not publish the verified discovery root")
	}
	return map[string]any{
		"retryAfterStatus":         http.StatusOK,
		"retryAfterRequests":       requests,
		"retryAfterElapsedMillis":  elapsed.Milliseconds(),
		"retryAfterRootSolidified": true,
	}, nil
}

func verifyBackgroundRetryRecovery(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	const reference = "minimal-background-retry"
	manifestBody, headers, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("background retry discovery read failed: %w", err)
	}
	if headers.Get("Docker-Content-Digest") != support.Digest(manifestBody) {
		return nil, fmt.Errorf("background retry discovery digest does not match manifest bytes")
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil || manifest.Config == nil || len(manifest.Layers) != 0 {
		return nil, fmt.Errorf("background retry fixture manifest has invalid config or layers")
	}
	task, err := waitForSolidifiedTask(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, err
	}
	if testkit.Int(task["retryCount"]) != 1 || testkit.String(task["errorClass"]) != "" || task["retryable"] == true || task["rootPublished"] != true {
		return nil, fmt.Errorf("background retry task did not recover cleanly: retryCount=%d errorClass=%q retryable=%v rootPublished=%v", testkit.Int(task["retryCount"]), testkit.String(task["errorClass"]), task["retryable"], task["rootPublished"])
	}
	rootDigest := support.Digest(manifestBody)
	if testkit.String(task["rootDigest"]) != rootDigest || testkit.Map(task["checkpoints"])[manifest.Config.Digest] != "verified" || testkit.Map(task["checkpoints"])[rootDigest] != "verified" {
		return nil, fmt.Errorf("background retry recovery omitted root or config checkpoint")
	}
	blob, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("background retry config blob is not locally readable: %w", err)
	}
	if string(blob) != "upstream-background-retry-config" {
		return nil, fmt.Errorf("background retry config blob bytes changed")
	}
	requests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	if requests != 4 {
		return nil, fmt.Errorf("background retry fixture requests = %d, want three foreground attempts and one background recovery", requests)
	}
	return map[string]any{
		"backgroundRetryTaskStatus":     testkit.String(task["status"]),
		"backgroundRetryTaskRetryCount": testkit.Int(task["retryCount"]),
		"backgroundRetryErrorCleared":   testkit.String(task["errorClass"]) == "" && task["retryable"] != true,
		"backgroundRetryRootPublished":  task["rootPublished"],
		"backgroundRetryRootDigest":     rootDigest,
		"backgroundRetryBlobDigest":     manifest.Config.Digest,
		"backgroundRetryBlobBytesMatch": true,
		"backgroundRetryBlobRequests":   requests,
	}, nil
}

type nestedExpectedObject struct {
	body      []byte
	mediaType string
	platform  map[string]string
}
