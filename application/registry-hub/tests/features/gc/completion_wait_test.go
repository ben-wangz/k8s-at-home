package gc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForGCWaitsForGateReopenAfterExecutorSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reads := 0
	run, err := waitForGCStatus(ctx, func(context.Context) (map[string]any, error) {
		reads++
		gate := "closed"
		if reads > 1 {
			gate = "open"
		}
		return map[string]any{"gate": map[string]any{"state": gate}, "run": map[string]any{"state": "succeeded"}}, nil
	})
	if err != nil || reads != 2 || run["_gateState"] != "open" {
		t.Fatalf("GC completion ignored asynchronous gate reopening: run=%v reads=%d err=%v", run, reads, err)
	}
}

func TestWaitForGCRejectsPermanentlyClosedGateAndFailedRuns(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "failed_unknown"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := waitForGCStatus(ctx, func(context.Context) (map[string]any, error) {
				return map[string]any{"gate": map[string]any{"state": "closed"}, "run": map[string]any{"state": state}}, nil
			})
			if err == nil || state == "succeeded" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("incomplete or failed GC accepted: state=%s err=%v", state, err)
			}
		})
	}
}
