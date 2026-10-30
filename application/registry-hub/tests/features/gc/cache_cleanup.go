package gc

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func cleanupSolidificationCache(ctx context.Context, environment testkit.CaseContext, taskID, digest string) (map[string]any, error) {
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
	// Automatic cleanup may win the race; verify the target instead of the deletion count.
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		missing, err := protectedBlobCacheMissing(ctx, environment, taskID, digest)
		if err != nil {
			return nil, err
		}
		if missing {
			result["targetCacheMissing"] = true
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("protected blob %s remained in cache", digest)
		case <-time.After(time.Second):
		}
	}
}

func protectedBlobCacheMissing(ctx context.Context, environment testkit.CaseContext, taskID, digest string) (bool, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/dependencies?limit=100", nil, nil)
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return false, fmt.Errorf("read protected blob cache evidence returned %d, want 200", response.StatusCode)
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &page); err != nil {
		return false, err
	}
	return cacheDependencyMissing(page, digest)
}

func cacheDependencyMissing(page map[string]any, digest string) (bool, error) {
	if testkit.String(page["consistency"]) != "live" {
		return false, fmt.Errorf("cache cleanup evidence is not live")
	}
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["digest"]) != digest || testkit.String(item["role"]) != "blob" {
			continue
		}
		if len(testkit.Map(item["readErrors"])) != 0 {
			return false, fmt.Errorf("protected blob cache evidence contains read errors")
		}
		return testkit.String(item["cacheState"]) == "missing", nil
	}
	return false, fmt.Errorf("protected blob %s is absent from the fixture's dependency page", digest)
}
