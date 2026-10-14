package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"time"
)

func verifySharedBlobWaiter(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string) (map[string]any, error) {
	body := []byte("minimal-shared-object-waiter")
	digest := support.Digest(body)
	path := "/v2/registry-hub-test/fixture/blobs/" + digest
	countBefore, err := countFixturePath(ctx, environment, path)
	if err != nil {
		return nil, err
	}
	if countBefore != 0 {
		return nil, fmt.Errorf("shared blob fixture request count before test = %d, want 0", countBefore)
	}
	leaderRegistry := *registry
	leaderRegistry.Client = testkit.NewHTTPClient(50*time.Second, nil)
	leaderCtx, cancelLeader := context.WithTimeout(ctx, 50*time.Second)
	defer cancelLeader()
	type leaderResult struct {
		status int
		body   []byte
		err    error
	}
	leaderDone := make(chan leaderResult, 1)
	leaderStarted := time.Now()
	go func() {
		response, requestErr := leaderRegistry.Get(leaderCtx, repository, "/blobs/"+digest, nil)
		if requestErr != nil {
			leaderDone <- leaderResult{err: requestErr}
			return
		}
		content, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		leaderDone <- leaderResult{status: response.StatusCode, body: content, err: readErr}
	}()
	if err := waitForFixturePathCount(ctx, environment, path, 1); err != nil {
		return nil, fmt.Errorf("shared blob leader did not reach upstream fixture: %w", err)
	}
	waiterStarted := time.Now()
	waiterResponse, err := leaderRegistry.Get(ctx, repository, "/blobs/"+digest, nil)
	if err != nil {
		return nil, fmt.Errorf("shared blob waiter request failed: %w", err)
	}
	waiterElapsed := time.Since(waiterStarted)
	waiterStatus := waiterResponse.StatusCode
	retryAfter := waiterResponse.Header.Get("Retry-After")
	waiterBody, readErr := io.ReadAll(waiterResponse.Body)
	_ = waiterResponse.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read shared blob waiter response: %w", readErr)
	}
	if waiterStatus != http.StatusServiceUnavailable || retryAfter != "5" {
		return nil, fmt.Errorf("shared blob waiter returned status %d and Retry-After %q, want 503 and 5", waiterStatus, retryAfter)
	}
	if waiterElapsed < 29*time.Second || waiterElapsed > 33*time.Second {
		return nil, fmt.Errorf("shared blob waiter waited %s, want approximately 30 seconds", waiterElapsed)
	}
	requestsAfterWaiter, err := countFixturePath(ctx, environment, path)
	if err != nil {
		return nil, err
	}
	if requestsAfterWaiter != 1 {
		return nil, fmt.Errorf("shared blob upstream requests while waiter was pending = %d, want 1", requestsAfterWaiter)
	}
	var leader leaderResult
	select {
	case leader = <-leaderDone:
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("shared blob leader did not finish after waiter timeout")
	}
	if leader.err != nil || leader.status != http.StatusOK || string(leader.body) != string(body) {
		return nil, fmt.Errorf("shared blob leader result status=%d err=%v bodyMatches=%t", leader.status, leader.err, string(leader.body) == string(body))
	}
	requestsAfterLeader, err := countFixturePath(ctx, environment, path)
	if err != nil {
		return nil, err
	}
	if requestsAfterLeader != 1 {
		return nil, fmt.Errorf("shared blob total upstream requests = %d, want 1", requestsAfterLeader)
	}
	return map[string]any{
		"sharedBlobDigest":               digest,
		"sharedBlobWaiterStatus":         waiterStatus,
		"sharedBlobWaiterRetryAfter":     retryAfter,
		"sharedBlobWaiterElapsedSeconds": waiterElapsed.Seconds(),
		"sharedBlobWaiterBodyBytes":      len(waiterBody),
		"sharedBlobUpstreamRequests":     requestsAfterLeader,
		"sharedBlobLeaderStatus":         leader.status,
		"sharedBlobLeaderBodyMatches":    true,
		"sharedBlobLeaderElapsedSeconds": time.Since(leaderStarted).Seconds(),
	}, nil
}

func waitForFixturePathCount(ctx context.Context, environment testkit.CaseContext, path string, expected int) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := countFixturePath(ctx, environment, path)
		if err != nil {
			return err
		}
		if count >= expected {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("fixture request count for %q remained %d, want %d", path, count, expected)
		case <-ticker.C:
		}
	}
}

func verifyRequestDeadline(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, remoteRepository, upstreamRepository string) (result map[string]any, returnErr error) {
	defer restoreRequestTimeout(&returnErr, environment)
	if err := setBackendEnvironment(ctx, environment, "REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=2"); err != nil {
		return nil, fmt.Errorf("set controlled request deadline: %w", err)
	}
	started := time.Now()
	response, err := registry.Get(ctx, remoteRepository, "/manifests/minimal-request-deadline", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return nil, fmt.Errorf("request deadline call failed before returning a response: %w", err)
	}
	defer response.Body.Close()
	elapsed := time.Since(started)
	if response.StatusCode != http.StatusGatewayTimeout {
		return nil, fmt.Errorf("request deadline returned %d, want 504", response.StatusCode)
	}
	envelope, err := support.DecodeError(response)
	if err != nil {
		return nil, fmt.Errorf("decode request deadline error response: %w", err)
	}
	errors := testkit.Slice(envelope["errors"])
	if len(errors) != 1 || testkit.String(testkit.Map(errors[0])["code"]) != "UNAVAILABLE" || response.Header.Get("Retry-After") != "5" {
		return nil, fmt.Errorf("request deadline error envelope or Retry-After header is invalid")
	}
	if elapsed < time.Second || elapsed > 10*time.Second {
		return nil, fmt.Errorf("request deadline elapsed %s, want approximately 2 seconds", elapsed)
	}
	requests, err := countFixtureRequests(ctx, environment, "minimal-request-deadline")
	if err != nil {
		return nil, err
	}
	if requests != 1 {
		return nil, fmt.Errorf("request deadline fixture requests = %d, want one attempt before cancellation", requests)
	}
	task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, "minimal-request-deadline")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["errorClass"]) != "upstream" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true || task["retryable"] == true {
		return nil, fmt.Errorf("request deadline task has invalid terminal state: class=%q root=%q published=%v retryable=%v", testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], task["retryable"])
	}
	return map[string]any{
		"requestDeadlineStatus":          response.StatusCode,
		"requestDeadlineElapsedMillis":   elapsed.Milliseconds(),
		"requestDeadlineFixtureRequests": requests,
		"requestDeadlineErrorCode":       "UNAVAILABLE",
		"requestDeadlineRetryAfter":      response.Header.Get("Retry-After"),
		"requestDeadlineTaskStatus":      testkit.String(task["status"]),
		"requestDeadlineTaskErrorClass":  testkit.String(task["errorClass"]),
		"requestDeadlineRootUnpublished": task["rootPublished"] != true,
		"requestDeadlineRestored":        true,
	}, nil
}

func verifyHeadRequestDeadline(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (result map[string]any, returnErr error) {
	defer restoreRequestTimeout(&returnErr, environment)
	if err := setBackendEnvironment(ctx, environment, "REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=2"); err != nil {
		return nil, fmt.Errorf("set controlled HEAD request deadline: %w", err)
	}
	const reference = "minimal-head-request-deadline"
	started := time.Now()
	response, err := registry.Head(ctx, repository, "/manifests/"+reference, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return nil, fmt.Errorf("HEAD request deadline call failed before returning a response: %w", err)
	}
	body, readErr := io.ReadAll(response.Body)
	status := response.StatusCode
	retryAfter := response.Header.Get("Retry-After")
	_ = response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	elapsed := time.Since(started)
	if status != http.StatusGatewayTimeout || retryAfter != "5" || len(body) != 0 {
		return nil, fmt.Errorf("HEAD request deadline returned status=%d retry-after=%q body-bytes=%d, want 504/5/0", status, retryAfter, len(body))
	}
	if elapsed < time.Second || elapsed > 10*time.Second {
		return nil, fmt.Errorf("HEAD request deadline elapsed %s, want approximately 2 seconds", elapsed)
	}
	requests, err := countFixtureRequests(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	methods, err := fixtureMethodsForReference(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	tasks, err := countTasksForReference(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, err
	}
	if requests != 1 || len(methods) != 1 || methods[0] != http.MethodHead || tasks != 0 {
		return nil, fmt.Errorf("HEAD request deadline fixture state is invalid: requests=%d methods=%q tasks=%d", requests, methods, tasks)
	}
	return map[string]any{
		"headRequestDeadlineStatus":          status,
		"headRequestDeadlineElapsedMillis":   elapsed.Milliseconds(),
		"headRequestDeadlineRetryAfter":      retryAfter,
		"headRequestDeadlineBodyBytes":       len(body),
		"headRequestDeadlineFixtureRequests": requests,
		"headRequestDeadlineMethods":         methods,
		"headRequestDeadlineTaskCount":       tasks,
		"headRequestDeadlineRestored":        true,
	}, nil
}
