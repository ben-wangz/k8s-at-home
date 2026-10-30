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
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	lastStatus := "not_observed"
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return nil, err
		}
		for _, item := range testkit.Slice(page["items"]) {
			task := testkit.Map(item)
			if testkit.String(task["reference"]) != reference {
				continue
			}
			lastStatus = testkit.String(task["status"])
			if lastStatus == expectedStatus {
				id := testkit.String(task["id"])
				if id == "" {
					return nil, fmt.Errorf("upstream import task %q has no ID", reference)
				}
				detailResponse, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(id), nil, nil)
				if err != nil {
					return nil, err
				}
				var detail map[string]any
				if _, err := support.DecodeJSON(detailResponse, 4<<20, &detail); err != nil {
					return nil, err
				}
				return detail, nil
			}
			if lastStatus == "failed" || lastStatus == "cancelled" || lastStatus == "solidified" {
				return nil, fmt.Errorf("upstream import task %q ended in %s, want %s (%s)", reference, lastStatus, expectedStatus, testkit.String(task["errorClass"]))
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
