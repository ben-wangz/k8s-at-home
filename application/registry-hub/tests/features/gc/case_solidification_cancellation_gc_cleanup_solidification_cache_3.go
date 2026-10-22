package gc

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

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
