package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"time"
)

func verifyQueueSaturation(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, localRepository string, localManifest []byte) (result map[string]any, returnErr error) {
	activeBefore, err := countActiveTasks(ctx, environment)
	if err != nil {
		return nil, err
	}
	queueLimit := activeBefore + 1
	if queueLimit < 1 {
		queueLimit = 1
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := setBackendEnvironment(restoreCtx, environment,
			"REGISTRY_HUB_PENDING_TASKS=1024",
			"REGISTRY_HUB_WORKERS=4",
			"REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=300",
		); err != nil {
			restoreErr := fmt.Errorf("restore default task queue settings: %w", err)
			if returnErr == nil {
				returnErr = restoreErr
			} else {
				returnErr = fmt.Errorf("%w; %v", returnErr, restoreErr)
			}
		}
	}()
	if err := setBackendEnvironment(ctx, environment,
		fmt.Sprintf("REGISTRY_HUB_PENDING_TASKS=%d", queueLimit),
		"REGISTRY_HUB_WORKERS=1",
		"REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=300",
	); err != nil {
		return nil, fmt.Errorf("set controlled task queue capacity: %w", err)
	}

	const blockedReference = "minimal-queue-block"
	const rejectedReference = "minimal-queue-rejected"
	remoteRepository := "github.com/registry-hub-test/fixture"
	blockedRequestsBefore, err := countFixtureRequests(ctx, environment, blockedReference)
	if err != nil {
		return nil, err
	}
	rejectedRequestsBefore, err := countFixtureRequests(ctx, environment, rejectedReference)
	if err != nil {
		return nil, err
	}
	blockedContext, cancelBlocked := context.WithCancel(ctx)
	defer cancelBlocked()
	blockedDone := make(chan error, 1)
	go func() {
		_, _, requestErr := support.ReadManifest(blockedContext, registry, remoteRepository, blockedReference, support.OCIManifestMediaType)
		blockedDone <- requestErr
	}()
	if err := waitForFixtureRequestCount(ctx, environment, blockedReference, blockedRequestsBefore+1); err != nil {
		cancelBlocked()
		return nil, fmt.Errorf("blocked upstream task did not reach fixture: %w", err)
	}
	activeWhileBlocked, err := countActiveTasks(ctx, environment)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	if activeWhileBlocked != queueLimit {
		cancelBlocked()
		return nil, fmt.Errorf("active tasks while upstream is blocked = %d, want queue capacity %d", activeWhileBlocked, queueLimit)
	}

	response, err := registry.Get(ctx, remoteRepository, "/manifests/"+rejectedReference, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	queueStatus := response.StatusCode
	queueRetryAfter := response.Header.Get("Retry-After")
	envelope, decodeErr := support.DecodeError(response)
	response.Body.Close()
	if decodeErr != nil {
		cancelBlocked()
		return nil, decodeErr
	}
	apiErrors := testkit.Slice(envelope["errors"])
	if queueStatus != http.StatusServiceUnavailable || queueRetryAfter != "5" || len(apiErrors) != 1 || testkit.String(testkit.Map(apiErrors[0])["code"]) != "UNAVAILABLE" {
		cancelBlocked()
		return nil, fmt.Errorf("full task queue response is invalid: status=%d retry-after=%q errors=%v", queueStatus, queueRetryAfter, envelope["errors"])
	}
	rejectedRequestsAfter, err := countFixtureRequests(ctx, environment, rejectedReference)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	rejectedTaskCount, err := countTasksForReference(ctx, environment, "github.com", "registry-hub-test/fixture", rejectedReference)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	if rejectedRequestsAfter != rejectedRequestsBefore || rejectedTaskCount != 0 {
		cancelBlocked()
		return nil, fmt.Errorf("queue-rejected request reached upstream or created a task: requests=%d tasks=%d", rejectedRequestsAfter-rejectedRequestsBefore, rejectedTaskCount)
	}
	fixtureLogsBeforeLocal, err := fixtureLogs(ctx, environment)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	localBody, _, err := support.ReadManifest(ctx, registry, localRepository, "minimal", support.OCIManifestMediaType)
	if err != nil {
		cancelBlocked()
		return nil, fmt.Errorf("local manifest read failed while queue is full: %w", err)
	}
	if string(localBody) != string(localManifest) {
		cancelBlocked()
		return nil, fmt.Errorf("local manifest bytes changed while queue is full")
	}
	fixtureLogsAfterLocal, err := fixtureLogs(ctx, environment)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	if fixtureLogsAfterLocal != fixtureLogsBeforeLocal {
		cancelBlocked()
		return nil, fmt.Errorf("local manifest hit reached the upstream fixture while queue is full")
	}
	localRequests, err := countFixtureRequests(ctx, environment, blockedReference)
	if err != nil {
		cancelBlocked()
		return nil, err
	}
	if localRequests != blockedRequestsBefore+1 {
		cancelBlocked()
		return nil, fmt.Errorf("local manifest hit changed blocked fixture request count to %d", localRequests)
	}

	cancelBlocked()
	select {
	case requestErr := <-blockedDone:
		if requestErr == nil {
			return nil, fmt.Errorf("blocked upstream client request succeeded after cancellation")
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("blocked upstream client request did not stop after cancellation")
	}
	blockedTask, err := waitForFailedTask(ctx, environment, "github.com", "registry-hub-test/fixture", blockedReference)
	if err != nil {
		return nil, err
	}
	if testkit.String(blockedTask["rootDigest"]) != "" || blockedTask["rootPublished"] == true {
		return nil, fmt.Errorf("cancelled queue task unexpectedly has a root")
	}
	activeAfter, err := waitForActiveTaskCount(ctx, environment, activeBefore)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"queueCapacity":                   queueLimit,
		"queueActiveTasksBefore":          activeBefore,
		"queueActiveTasksWhileBlocked":    activeWhileBlocked,
		"queueFullStatus":                 queueStatus,
		"queueFullRetryAfter":             queueRetryAfter,
		"queueRejectedFixtureRequests":    rejectedRequestsAfter - rejectedRequestsBefore,
		"queueRejectedTaskCount":          rejectedTaskCount,
		"queueLocalHitStatus":             http.StatusOK,
		"queueLocalHitBytesMatch":         true,
		"queueBlockedTaskStatus":          testkit.String(blockedTask["status"]),
		"queueBlockedTaskErrorClass":      testkit.String(blockedTask["errorClass"]),
		"queueBlockedTaskRootUnpublished": blockedTask["rootPublished"] != true,
		"queueActiveTasksAfterRelease":    activeAfter,
	}, nil
}

func countActiveTasks(ctx context.Context, environment testkit.CaseContext) (int, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=1000", nil, nil)
	if err != nil {
		return 0, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return 0, err
	}
	count := 0
	for _, item := range testkit.Slice(page["items"]) {
		status := testkit.String(testkit.Map(item)["status"])
		if status == "pending" || status == "running" || status == "retry_wait" {
			count++
		}
	}
	return count, nil
}

func waitForActiveTaskCount(ctx context.Context, environment testkit.CaseContext, expected int) (int, error) {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := countActiveTasks(ctx, environment)
		if err != nil {
			return 0, err
		}
		if count <= expected {
			return count, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-deadline.C:
			return count, fmt.Errorf("active task count remained %d, want at most %d", count, expected)
		case <-ticker.C:
		}
	}
}

func waitForFixtureRequestCount(ctx context.Context, environment testkit.CaseContext, reference string, expected int) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := countFixtureRequests(ctx, environment, reference)
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
			return fmt.Errorf("fixture request count for %q remained %d, want %d", reference, count, expected)
		case <-ticker.C:
		}
	}
}
