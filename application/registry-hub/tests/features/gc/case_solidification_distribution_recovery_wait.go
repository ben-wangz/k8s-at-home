package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func blobTaskIsSolidified(task map[string]any) bool {
	return testkit.String(task["status"]) == "solidified"
}

func waitForBlobTaskSolidified(ctx context.Context, environment testkit.CaseContext, initial map[string]any) (map[string]any, error) {
	taskID := testkit.String(initial["id"])
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	current := initial
	for {
		status := testkit.String(current["status"])
		if blobTaskIsSolidified(current) {
			return current, nil
		}
		if status == "failed" || status == "cancelled" {
			return current, fmt.Errorf("blob task %s ended in %s before anchoring", taskID, status)
		}
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-deadline.C:
			return current, fmt.Errorf("blob task %s remained %s while awaiting manifest anchoring", taskID, status)
		case <-time.After(500 * time.Millisecond):
			updated, err := taskDetailByID(ctx, environment, taskID)
			if err != nil {
				return current, fmt.Errorf("read blob task %s while awaiting anchoring: %w", taskID, err)
			}
			current = updated
		}
	}
}
