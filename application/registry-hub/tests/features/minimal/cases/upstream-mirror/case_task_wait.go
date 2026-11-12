package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"time"
)

func waitForTaskStatusWithin(ctx context.Context, environment testkit.CaseContext, authority, repository, reference, expectedStatus string, timeout time.Duration) (map[string]any, error) {
	query := url.Values{
		"authority":  {authority},
		"repository": {repository},
		"digest":     {reference},
		"limit":      {"100"},
	}
	return waitTaskStatus(ctx, query, reference, expectedStatus, timeout, func(path string) (map[string]any, error) {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
		if err != nil {
			return nil, err
		}
		var document map[string]any
		_, err = support.DecodeJSON(response, 4<<20, &document)
		return document, err
	})
}

func waitTaskStatus(ctx context.Context, query url.Values, reference, expectedStatus string, timeout time.Duration, read func(string) (map[string]any, error)) (map[string]any, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	lastStatus, id, token := "not_observed", "", ""
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("upstream import task %q did not reach %s within %s (last status %s)", reference, expectedStatus, timeout, lastStatus)
		default:
		}
		path := "/api/v1/tasks?" + query.Encode()
		if token != "" {
			path += "&token=" + url.QueryEscape(token)
		}
		if id != "" {
			path = "/api/v1/tasks/" + url.PathEscape(id) + "?graphLimit=1&ownerLimit=1"
		}
		page, err := read(path)
		if err != nil {
			return nil, err
		}
		items := testkit.Slice(page["items"])
		if id != "" {
			items = []any{page}
		}
		for _, item := range items {
			task := testkit.Map(item)
			if testkit.String(task["reference"]) != reference {
				continue
			}
			id = testkit.String(task["id"])
			if id == "" {
				return nil, fmt.Errorf("upstream import task %q has no ID", reference)
			}
			lastStatus = testkit.String(task["status"])
			if lastStatus == expectedStatus {
				return read("/api/v1/tasks/" + url.PathEscape(id))
			}
			if lastStatus == "failed" || lastStatus == "cancelled" || lastStatus == "solidified" {
				return nil, fmt.Errorf("upstream import task %q ended in %s, want %s (%s)", reference, lastStatus, expectedStatus, testkit.String(task["errorClass"]))
			}
		}
		if id == "" {
			token = testkit.String(page["nextToken"])
			if token != "" {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("upstream import task %q did not reach %s within %s (last status %s)", reference, expectedStatus, timeout, lastStatus)
		case <-time.After(time.Second):
		}
	}
}
