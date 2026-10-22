package gc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifySolidificationCancellationGC(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string) (map[string]any, error) {
	referenceHash := strings.TrimPrefix(support.Digest([]byte(environment.RunID)), "sha256:")
	reference := "gc-cancel-" + referenceHash[:16]
	startedAt := time.Now().UTC()
	manifestBytes, _, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("read cancellation fixture manifest: %w", err)
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Config == nil || len(manifest.Layers) != 1 || manifest.Config.Data == "" {
		return nil, fmt.Errorf("cancellation fixture manifest is incomplete")
	}
	configBytes, err := base64.StdEncoding.DecodeString(manifest.Config.Data)
	if err != nil || len(configBytes) != manifest.Config.Size || support.Digest(configBytes) != manifest.Config.Digest {
		return nil, fmt.Errorf("cancellation fixture inline config is invalid")
	}
	clientBytes, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("inline config read failed: %w", err)
	}
	if !bytes.Equal(clientBytes, configBytes) {
		return nil, fmt.Errorf("inline config read returned unexpected bytes")
	}
	tasks, err := waitForProtectedSolidification(ctx, environment, repository, reference, manifest.Config.Digest, startedAt)
	if err != nil {
		return nil, err
	}
	cacheCleanup, err := cleanupSolidificationCache(ctx, environment)
	if err != nil {
		return nil, err
	}
	blocked, err := rejectGCWhileSolidificationProtected(ctx, environment)
	if err != nil {
		return nil, err
	}
	cancelled, err := cancelSolidificationTasks(ctx, environment, repository, reference, manifest.Config.Digest, startedAt)
	if err != nil {
		return nil, err
	}
	gate, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(gate); err != nil {
		return nil, err
	}
	gcStarted, err := startGC(ctx, environment)
	if err != nil {
		return nil, err
	}
	gcFinished, err := waitForGC(ctx, environment)
	if err != nil {
		return nil, err
	}
	if testkit.String(gcFinished["id"]) != testkit.String(gcStarted["id"]) || testkit.String(gcFinished["state"]) != "succeeded" {
		return nil, fmt.Errorf("GC after cancellation did not complete successfully")
	}
	gcJob, err := verifyGCJob(ctx, environment, gcStarted, gcFinished)
	if err != nil {
		return nil, err
	}
	if err := cleanupGCJob(ctx, environment, testkit.String(gcJob["name"]), testkit.String(gcJob["uid"])); err != nil {
		return nil, err
	}
	readSince := time.Now().UTC()
	upstreamBefore, err := fixtureBlobRequestCount(ctx, environment, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	refetched, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("post-GC client blob read failed: %w", err)
	}
	if !bytes.Equal(refetched, configBytes) {
		return nil, fmt.Errorf("post-GC client blob read returned unexpected bytes")
	}
	upstreamAfter, err := fixtureBlobRequestCount(ctx, environment, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	distributionMisses, err := distributionBlobMissCount(ctx, environment, manifest.Config.Digest, readSince)
	if err != nil {
		return nil, err
	}
	if upstreamAfter != upstreamBefore+1 || distributionMisses == 0 {
		return nil, fmt.Errorf("post-GC read did not prove Distribution and cache misses: upstream requests %d -> %d, Distribution misses %d", upstreamBefore, upstreamAfter, distributionMisses)
	}
	newBlobTask, err := waitForNewBlobTask(ctx, environment, repository, manifest.Config.Digest, readSince, testkit.String(testkit.Map(tasks["blobTask"])["roundID"]))
	if err != nil {
		return nil, err
	}
	newBlobTaskID := testkit.String(newBlobTask["id"])
	newBlobTask, err = cancelAndWaitForTask(ctx, environment, newBlobTaskID)
	if err != nil {
		return nil, err
	}
	returnStatus, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(returnStatus); err != nil {
		return nil, err
	}
	return map[string]any{
		"reference": reference, "taskID": testkit.String(testkit.Map(tasks["rootTask"])["id"]), "taskStatusAfterCancel": "cancelled",
		"cancelResponseStatus": testkit.Int(cancelled["cancelResponseStatus"]), "cancelledTasks": cancelled["taskIDs"],
		"cancelResponses": cancelled["responses"],
		"duplicateCacheEntriesRemovedWhileProtected": testkit.Int(cacheCleanup["cacheRemoved"]),
		"protectionSessionBlockedGC":                 blocked, "cancellationQuiesced": cancelled["quiesced"] == true,
		"gcGateOpenAfterCancellation": true,
		"gcAfterCancelID":             testkit.String(gcFinished["id"]), "unanchoredBlobDigest": manifest.Config.Digest,
		"distributionBlobMissesAfterGC": distributionMisses, "upstreamBlobRequestsBefore": upstreamBefore,
		"upstreamBlobRequestsAfter": upstreamAfter, "cacheAndDistributionMissRecoveredFromUpstream": true,
		"newSolidificationRoundAfterRefetch": true, "newBlobTaskID": newBlobTaskID,
		"newBlobTaskStatusAfterCleanup": testkit.String(newBlobTask["status"]),
	}, nil
}

func waitForProtectedSolidification(ctx context.Context, environment testkit.CaseContext, repository, reference, digest string, since time.Time) (map[string]any, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		rootTask, err := taskDetailForReference(ctx, environment, repository, reference)
		if err == nil {
			status := testkit.String(rootTask["status"])
			if status == "solidified" || status == "failed" || status == "cancelled" {
				return nil, fmt.Errorf("cancellation fixture task ended in %s before the protected state", status)
			}
			if rootTask["rootPublished"] == true || testkit.String(rootTask["rootDigest"]) == "" {
				return nil, fmt.Errorf("cancellation fixture root was published or not discovered")
			}
			blobTask, blobErr := taskDetailForBlobDigest(ctx, environment, repository, digest, since, "")
			if blobErr != nil && !strings.Contains(blobErr.Error(), "no matching blob task") {
				return nil, blobErr
			}
			unanchored, err := hasQuiescentUnanchoredSession(rootTask)
			if err != nil {
				return nil, err
			}
			if !unanchored && blobTask != nil {
				unanchored, err = hasQuiescentUnanchoredSession(blobTask)
				if err != nil {
					return nil, err
				}
			}
			localReads, err := distributionBlobReadCount(ctx, environment, digest, since)
			if err != nil {
				return nil, err
			}
			if unanchored && localReads > 0 && blobTask != nil && testkit.String(blobTask["status"]) == "verified_local" {
				return map[string]any{"rootTask": rootTask, "blobTask": blobTask}, nil
			}
		} else if !strings.Contains(err.Error(), "no matching task") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("cancellation fixture did not reach a quiescent unanchored state")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func taskDetailForBlobDigest(ctx context.Context, environment testkit.CaseContext, repository, digest string, since time.Time, excludedRound string) (map[string]any, error) {
	query := url.Values{
		"authority": {"github.com"}, "repository": {repositoryPath(repository)},
		"digest": {digest}, "since": {since.Add(-time.Second).Format(time.RFC3339)}, "limit": {"100"},
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["kind"]) != "blob" || testkit.String(item["blobDigest"]) != digest || excludedRound != "" && testkit.String(item["roundID"]) == excludedRound {
			continue
		}
		id := testkit.String(item["id"])
		return taskDetailByID(ctx, environment, id)
	}
	return nil, fmt.Errorf("no matching blob task for digest %s", digest)
}

func taskDetailForReference(ctx context.Context, environment testkit.CaseContext, repository, reference string) (map[string]any, error) {
	query := url.Values{"authority": {"github.com"}, "repository": {repository[strings.IndexByte(repository, '/')+1:]}, "digest": {reference}, "limit": {"100"}}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["reference"]) != reference {
			continue
		}
		id := testkit.String(item["id"])
		detail, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return nil, err
		}
		var result map[string]any
		if _, err := support.DecodeJSON(detail, 4<<20, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("no matching task for reference %s", reference)
}

func hasQuiescentUnanchoredSession(task map[string]any) (bool, error) {
	for _, raw := range testkit.Slice(task["protectionSessions"]) {
		session := testkit.Map(raw)
		if session["mayContainUnanchored"] == true && testkit.Int(session["pendingOperations"]) == 0 && session["uncertainRemoteResult"] != true {
			return true, nil
		}
	}
	return false, nil
}

func rejectGCWhileSolidificationProtected(ctx context.Context, environment testkit.CaseContext) (bool, error) {
	before, err := gcStatus(ctx, environment)
	if err != nil {
		return false, err
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc", nil, nil)
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusConflict {
		if response.StatusCode == http.StatusAccepted {
			var started map[string]any
			if _, decodeErr := support.DecodeJSON(response, 2<<20, &started); decodeErr == nil {
				_, _ = waitForGC(ctx, environment)
			}
		} else {
			response.Body.Close()
		}
		return false, fmt.Errorf("GC during unanchored solidification returned %d, want 409", response.StatusCode)
	}
	var envelope struct {
		Errors []struct {
			Code   string `json:"code"`
			Detail struct {
				Blockers []any `json:"blockers"`
			} `json:"detail"`
		} `json:"errors"`
	}
	if _, err := support.DecodeJSON(response, 2<<20, &envelope); err != nil {
		return false, err
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "GC_BLOCKED" || len(envelope.Errors[0].Detail.Blockers) == 0 {
		return false, fmt.Errorf("GC conflict did not report solidification blockers")
	}
	after, err := gcStatus(ctx, environment)
	if err != nil {
		return false, err
	}
	if testkit.String(testkit.Map(after["gate"])["state"]) != "open" || testkit.String(testkit.Map(after["run"])["id"]) != testkit.String(testkit.Map(before["run"])["id"]) {
		return false, fmt.Errorf("rejected GC changed gate state or started a GC run")
	}
	return true, nil
}

func requireNoProtectionBlockers(status map[string]any) error {
	if testkit.String(testkit.Map(status["gate"])["state"]) != "open" {
		return fmt.Errorf("GC gate did not reopen after solidification cancellation")
	}
	for _, raw := range testkit.Slice(status["sessions"]) {
		session := testkit.Map(raw)
		state := testkit.String(session["state"])
		if state == "protected" || state == "anchoring" || state == "stop_requested" || state == "quiescing" || state == "quiesced" || state == "recovery_required" {
			return fmt.Errorf("solidification protection session %s remains in %s after cancellation", testkit.String(session["id"]), state)
		}
	}
	return nil
}

func cancelAndWaitForTask(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := requestTaskCancellation(ctx, environment, taskID)
	if err != nil {
		return nil, err
	}
	result, err := waitForCancelledTask(ctx, environment, taskID)
	if err != nil {
		return nil, err
	}
	result["cancelResponseStatus"] = int64(testkit.Int(response["status"]))
	return result, nil
}

func requestTaskCancellation(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/cancel", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("cancel task %s returned %d, want 202 or an already-quiesced 200", taskID, response.StatusCode)
	}
	var task map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &task); err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusOK && (testkit.String(task["status"]) != "cancelled" || task["terminalCleanupComplete"] != true || task["gcProtectionActive"] == true) {
		return nil, fmt.Errorf("cancel task %s returned 200 without a terminal, unprotected state: %#v", taskID, task)
	}
	return map[string]any{
		"taskID": taskID, "status": int64(response.StatusCode), "responseTaskStatus": task["status"],
		"responseTerminalCleanupComplete": task["terminalCleanupComplete"], "responseGCProtectionActive": task["gcProtectionActive"],
	}, nil
}

func waitForCancelledTask(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		current, err := taskDetailByID(ctx, environment, taskID)
		if err != nil {
			return nil, err
		}
		if testkit.String(current["status"]) == "cancelled" && current["terminalCleanupComplete"] == true && current["gcProtectionActive"] != true {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("task %s did not quiesce after cancellation", taskID)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cleanupSolidificationCache(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/cleanup", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("cache cleanup returned %d, want 200", response.StatusCode)
	}
	var result map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &result); err != nil {
		return nil, err
	}
	if testkit.Int(result["cacheRemoved"]) == 0 {
		return nil, fmt.Errorf("protected blob duplicate cache data was not physically removed")
	}
	return result, nil
}

func cancelSolidificationTasks(ctx context.Context, environment testkit.CaseContext, repository, reference, digest string, since time.Time) (map[string]any, error) {
	query := url.Values{
		"authority": {"github.com"}, "repository": {repositoryPath(repository)},
		"since": {since.Add(-time.Second).Format(time.RFC3339)}, "limit": {"100"},
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	var matching []map[string]any
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["reference"]) != reference && (testkit.String(item["kind"]) != "blob" || testkit.String(item["blobDigest"]) != digest) {
			continue
		}
		status := testkit.String(item["status"])
		if status == "solidified" || status == "failed" || status == "cancelled" {
			continue
		}
		matching = append(matching, item)
	}
	if len(matching) == 0 {
		return nil, fmt.Errorf("no active solidification tasks matched the protected fixture")
	}
	sort.SliceStable(matching, func(i, j int) bool {
		return testkit.String(matching[i]["kind"]) == "root" && testkit.String(matching[j]["kind"]) != "root"
	})
	var taskIDs []string
	var responses []map[string]any
	for _, item := range matching {
		taskID := testkit.String(item["id"])
		current, err := taskDetailByID(ctx, environment, taskID)
		if err != nil {
			return nil, err
		}
		state := testkit.String(current["status"])
		if state == "solidified" || state == "failed" || state == "cancelled" {
			continue
		}
		response, err := requestTaskCancellation(ctx, environment, taskID)
		if err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, taskID)
		response["priorStatus"] = current["status"]
		response["priorGCProtectionActive"] = current["gcProtectionActive"]
		responses = append(responses, response)
	}
	if len(taskIDs) == 0 {
		return nil, fmt.Errorf("no active solidification tasks remained to cancel")
	}
	for _, taskID := range taskIDs {
		if _, err := waitForCancelledTask(ctx, environment, taskID); err != nil {
			return nil, err
		}
	}
	for _, response := range responses {
		if testkit.Int(response["status"]) == http.StatusAccepted {
			continue
		}
		if testkit.Int(response["status"]) != http.StatusOK || testkit.String(response["responseTaskStatus"]) != "cancelled" || response["responseTerminalCleanupComplete"] != true || response["responseGCProtectionActive"] == true {
			return nil, fmt.Errorf("solidification cancellation response was neither accepted nor already quiescent: %#v", response)
		}
	}
	firstStatus := testkit.Int(responses[0]["status"])
	return map[string]any{
		"taskIDs": taskIDs, "responses": responses, "cancelResponseStatus": firstStatus, "quiesced": true, "status": "cancelled",
	}, nil
}

func waitForNewBlobTask(ctx context.Context, environment testkit.CaseContext, repository, digest string, since time.Time, previousRound string) (map[string]any, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		task, err := taskDetailForBlobDigest(ctx, environment, repository, digest, since, previousRound)
		if err == nil {
			status := testkit.String(task["status"])
			if status == "verified_local" || status == "solidified" {
				return task, nil
			}
			if status == "failed" || status == "cancelled" {
				return nil, fmt.Errorf("post-GC solidification task ended in %s", status)
			}
		} else if !strings.Contains(err.Error(), "no matching blob task") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("post-GC client read did not create a new blob solidification round")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func repositoryPath(repository string) string {
	if index := strings.IndexByte(repository, '/'); index >= 0 {
		return repository[index+1:]
	}
	return repository
}

func taskDetailByID(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, nil)
	if err != nil {
		return nil, err
	}
	var task map[string]any
	_, err = support.DecodeJSON(response, 2<<20, &task)
	return task, err
}

func distributionBlobReadCount(ctx context.Context, environment testkit.CaseContext, digest string, since time.Time) (int, error) {
	lines, err := distributionLogs(ctx, environment, since)
	if err != nil {
		return 0, err
	}
	path := `http.request.uri="/v2/egithub_2ecom/registry-hub-test/fixture/blobs/` + digest + `"`
	count := 0
	for _, line := range lines {
		if strings.Contains(line, `http.request.method=GET`) && strings.Contains(line, path) && strings.Contains(line, `http.response.status=200`) {
			count++
		}
	}
	return count, nil
}

func distributionBlobMissCount(ctx context.Context, environment testkit.CaseContext, digest string, since time.Time) (int, error) {
	lines, err := distributionLogs(ctx, environment, since)
	if err != nil {
		return 0, err
	}
	path := `http.request.uri="/v2/egithub_2ecom/registry-hub-test/fixture/blobs/` + digest + `"`
	count := 0
	for _, line := range lines {
		if strings.Contains(line, `http.request.method=GET`) && strings.Contains(line, path) && strings.Contains(line, `http.response.status=404`) {
			count++
		}
	}
	return count, nil
}

func distributionLogs(ctx context.Context, environment testkit.CaseContext, since time.Time) ([]string, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname+"-registry", "-c", "registry", "--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=20000")
	if err != nil {
		return nil, fmt.Errorf("read Distribution request evidence: %w", err)
	}
	return strings.Split(string(result.Stdout), "\n"), nil
}

func fixtureBlobRequestCount(ctx context.Context, environment testkit.CaseContext, digest string) (int, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+environment.Release+"-external-upstream-proxy", "-c", "proxy", "--tail=2000")
	if err != nil {
		return 0, fmt.Errorf("read upstream fixture request evidence: %w", err)
	}
	marker := "host=github.com path=/v2/registry-hub-test/fixture/blobs/" + digest + " auth=absent"
	count := 0
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if strings.Contains(line, marker) {
			count++
		}
	}
	return count, nil
}
