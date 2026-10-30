package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func verifyClosureDepthLimit(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	root, digests, err := expectedClosureDepthFixture()
	if err != nil {
		return nil, err
	}
	rootBody, rootHeaders, err := support.ReadManifest(ctx, registry, repository, "minimal-depth-limit", support.OCIIndexMediaType)
	if err != nil {
		return nil, err
	}
	rootDigest := support.Digest(root.body)
	if string(rootBody) != string(root.body) || rootHeaders.Get("Docker-Content-Digest") != rootDigest {
		return nil, fmt.Errorf("closure-depth root bytes or digest differ from the fixture")
	}
	task, err := waitForTaskStatusWithin(ctx, environment, "github.com", upstreamRepository, "minimal-depth-limit", "failed", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if testkit.String(task["errorClass"]) != "closure_limit" || testkit.String(task["rootDigest"]) != rootDigest || task["rootPublished"] == true || task["retryable"] == true {
		return nil, fmt.Errorf("closure-depth task has invalid terminal state: class=%q root=%q published=%v retryable=%v", testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], task["retryable"])
	}
	checkpoints := testkit.Map(task["checkpoints"])
	graph := testkit.Map(task["graph"])
	graphNodes := testkit.Slice(task["graphNodes"])
	if checkpoints[rootDigest] != nil || len(checkpoints) != len(digests)-2 || len(graph) != len(checkpoints) || len(graphNodes) != len(digests)-1 {
		return nil, fmt.Errorf("closure-depth checkpoint boundary is invalid: root=%v checkpoints=%d graph=%d persisted=%d", checkpoints[rootDigest], len(checkpoints), len(graph), len(graphNodes))
	}
	validChildDigests := make(map[string]struct{}, len(digests)-2)
	for depth := 1; depth < len(digests)-1; depth++ {
		validChildDigests[digests[depth]] = struct{}{}
	}
	incompleteManifestDigests := make(map[string]struct{}, len(digests)-1)
	for _, digest := range digests[:len(digests)-1] {
		incompleteManifestDigests[digest] = struct{}{}
	}
	for _, raw := range graphNodes {
		node := testkit.Map(raw)
		digest := testkit.String(node["digest"])
		if _, ok := incompleteManifestDigests[digest]; !ok || testkit.String(node["role"]) != "manifest" || node["discoveryComplete"] == true || testkit.String(node["localState"]) != "discovering" {
			return nil, fmt.Errorf("closure-depth graph node %q is not an incomplete manifest in the verified child chain", digest)
		}
		delete(incompleteManifestDigests, digest)
	}
	if len(incompleteManifestDigests) != 0 {
		return nil, fmt.Errorf("closure-depth task omitted %d incomplete manifest graph nodes", len(incompleteManifestDigests))
	}
	for digest, checkpoint := range checkpoints {
		if _, ok := validChildDigests[digest]; !ok || checkpoint != "verified" {
			return nil, fmt.Errorf("closure-depth task checkpoint %q is outside the verified child chain", digest)
		}
	}
	rootRequests, err := countFixtureRequests(ctx, environment, "minimal-depth-limit")
	if err != nil {
		return nil, err
	}
	if rootRequests != 1 {
		return nil, fmt.Errorf("closure-depth root requests = %d, want 1", rootRequests)
	}
	for depth := 1; depth < len(digests)-1; depth++ {
		requests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/manifests/"+digests[depth])
		if err != nil {
			return nil, err
		}
		if requests != 1 {
			return nil, fmt.Errorf("closure-depth node %d requests = %d, want 1", depth, requests)
		}
	}
	firstRejectedRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/manifests/"+digests[len(digests)-1])
	if err != nil {
		return nil, err
	}
	if firstRejectedRequests != 0 {
		return nil, fmt.Errorf("manifest beyond the configured closure depth was fetched %d times", firstRejectedRequests)
	}
	return map[string]any{
		"closureDepthTaskStatus":         testkit.String(task["status"]),
		"closureDepthErrorClass":         testkit.String(task["errorClass"]),
		"closureDepthRootDigest":         rootDigest,
		"closureDepthRootUnpublished":    task["rootPublished"] != true,
		"closureDepthRetryable":          task["retryable"],
		"closureDepthCheckpointCount":    len(checkpoints),
		"closureDepthRootRequests":       rootRequests,
		"closureDepthFetchedChildCount":  len(digests) - 2,
		"closureDepthFirstRejectedCount": firstRejectedRequests,
	}, nil
}

func verifySuccessfulHead(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	response, err := registry.Head(ctx, repository, "/manifests/minimal-head-success", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return nil, err
	}
	headBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read successful HEAD response: %v %v", readErr, closeErr)
	}
	headMediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	headContentLength, err := strconv.Atoi(response.Header.Get("Content-Length"))
	if err != nil {
		return nil, fmt.Errorf("successful HEAD has invalid Content-Length: %w", err)
	}
	if response.StatusCode != http.StatusOK || len(headBody) != 0 || headMediaType != support.OCIManifestMediaType || headContentLength <= 0 || !strings.HasPrefix(response.Header.Get("Docker-Content-Digest"), "sha256:") {
		return nil, fmt.Errorf("successful HEAD metadata is incomplete: status=%d mediaType=%q length=%d body=%d digest=%q", response.StatusCode, headMediaType, headContentLength, len(headBody), response.Header.Get("Docker-Content-Digest"))
	}
	headTasks, err := countTasksForReference(ctx, environment, "github.com", upstreamRepository, "minimal-head-success")
	if err != nil {
		return nil, err
	}
	headMethods, err := fixtureMethodsForReference(ctx, environment, "minimal-head-success")
	if err != nil {
		return nil, err
	}
	if headTasks != 0 || len(headMethods) != 1 || headMethods[0] != http.MethodHead {
		return nil, fmt.Errorf("successful HEAD created a task or another upstream method: tasks=%d methods=%q", headTasks, headMethods)
	}
	manifestBody, manifestHeaders, err := support.ReadManifest(ctx, registry, repository, "minimal-head-success", support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("GET after successful HEAD failed: %w", err)
	}
	methodsAfterGet, err := fixtureMethodsForReference(ctx, environment, "minimal-head-success")
	if err != nil {
		return nil, err
	}
	if len(methodsAfterGet) != 2 || methodsAfterGet[0] != http.MethodHead || methodsAfterGet[1] != http.MethodGet || support.Digest(manifestBody) != response.Header.Get("Docker-Content-Digest") || len(manifestBody) != headContentLength || manifestHeaders.Get("Docker-Content-Digest") != response.Header.Get("Docker-Content-Digest") {
		return nil, fmt.Errorf("GET after successful HEAD did not fetch and match the manifest")
	}
	return map[string]any{
		"successfulHeadStatus":        response.StatusCode,
		"successfulHeadMediaType":     headMediaType,
		"successfulHeadContentLength": headContentLength,
		"successfulHeadDigest":        response.Header.Get("Docker-Content-Digest"),
		"successfulHeadBodyBytes":     len(headBody),
		"successfulHeadTaskCount":     headTasks,
		"successfulHeadMethods":       headMethods,
		"successfulHeadGetMethods":    methodsAfterGet,
		"successfulHeadCacheMiss":     len(methodsAfterGet) == 2 && methodsAfterGet[1] == http.MethodGet,
		"successfulHeadGetBytesMatch": string(manifestBody) != "" && support.Digest(manifestBody) == response.Header.Get("Docker-Content-Digest"),
	}, nil
}
