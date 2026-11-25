package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func TestHistoryCleanupRangePreviewExecutesExactSegmentAndSealsSelectedTimes(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{
		Username: "operator", Password: "secret", Permissions: []string{auth.HistoryCleanup, auth.Observe},
	}}}
	ctx := context.Background()
	day := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
	from, to := day.Add(10*time.Hour), day.Add(11*time.Hour)
	inside := diagnostics.Event{ID: "range-inside", OccurredAt: from.Add(30 * time.Minute)}
	changed := diagnostics.Event{ID: "range-changed", OccurredAt: from.Add(45 * time.Minute)}
	outside := diagnostics.Event{ID: "range-outside", OccurredAt: to.Add(30 * time.Minute)}
	for _, event := range []diagnostics.Event{inside, changed, outside} {
		if err := putHistoryCleanupJSON(ctx, f.service.store, diagnostics.EventKey(event), event, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 37 {
		at := from.Add(time.Duration(i) * time.Second)
		if i == 0 {
			at = from.Add(-time.Second)
		}
		event := diagnostics.Event{ID: fmt.Sprintf("fixture-%02d", i), OccurredAt: at}
		if err := putHistoryCleanupJSON(ctx, f.service.store, diagnostics.EventKey(event), event, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	batchEvents := []diagnostics.Event{
		{ID: "batch-inside", OccurredAt: from.Add(20 * time.Minute)},
		{ID: "batch-outside", OccurredAt: to.Add(20 * time.Minute)},
	}
	batchContent, err := json.Marshal(batchEvents)
	if err != nil {
		t.Fatal(err)
	}
	contentHash := sha256.Sum256(batchContent)
	date := from.Format("20060102")
	batch := diagnostics.EventBatch{
		SchemaVersion: 2, ID: diagnostics.ID("event-batch", date, hex.EncodeToString(contentHash[:])), Date: date,
		ContentSHA256: hex.EncodeToString(contentHash[:]), CreatedAt: time.Now().UTC(), Events: batchEvents,
	}
	batchKey := diagnostics.EventBatchKey(date, batch.ID)
	if err := putHistoryCleanupJSON(ctx, f.service.store, batchKey, batch, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}

	requestBody, err := json.Marshal(historyCleanupRangeRequest{Category: "diagnostic_events", From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	created := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup/previews", "", string(requestBody))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create range preview: status=%d body=%s", created.Code, created.Body.String())
	}
	var preview historyCleanupRangePreview
	if err := json.Unmarshal(created.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewCompleted := false
	for range 16 {
		progress, done, stepErr := f.service.historyCleanupRangePreviewStep(ctx, &job)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		job.Progress, err = json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			previewCompleted = true
			break
		}
	}
	if !previewCompleted {
		t.Fatal("range preview did not complete within the bounded test steps")
	}
	completedPreview, err := f.service.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	counts := completedPreview.Counts
	if completedPreview.Status != "completed" || completedPreview.SegmentItems != 38 || counts.Eligible != 38 || counts.Protected != 1 || counts.SkipReasons["cross_range_batch"] != 1 ||
		completedPreview.Limits.StorageRequests != historyCleanupStorageRequests || completedPreview.Limits.MaxStepSeconds != int64(historyCleanupRangeBudget.Seconds()) {
		t.Fatalf("completed range preview=%+v", completedPreview)
	}
	if err := inventory.Update(ctx, f.service.store, diagnostics.EventKey(changed), func(current *diagnostics.Event) error {
		current.Result = "changed-after-preview"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	submitBody, err := json.Marshal(historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: completedPreview.SegmentID})
	if err != nil {
		t.Fatal(err)
	}
	accepted := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup", "range-segment-1", string(submitBody))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("submit range segment: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	var task historyCleanupTaskView
	if err := json.Unmarshal(accepted.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.backgroundQueue().Read(ctx, historyCleanupWorkKind, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCompleted := false
	for range 48 {
		progress, done, stepErr := f.service.historyCleanupStep(ctx, &job)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		job.Progress, err = json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			cleanupCompleted = true
			break
		}
	}
	if !cleanupCompleted {
		t.Fatal("segment cleanup did not complete within the bounded test steps")
	}
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, f.service.store, historyCleanupOperationKey(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	executionCounts := operation.Counts["diagnostic_events"]
	if operation.Status != "completed" || executionCounts.Deleted != 37 || executionCounts.Protected != 1 || executionCounts.SkipReasons["version_changed"] != 1 {
		t.Fatalf("completed range cleanup=%+v counts=%+v", operation, executionCounts)
	}
	if _, err := f.service.store.Head(ctx, diagnostics.EventKey(inside)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("selected event remains: %v", err)
	}
	for _, key := range []string{diagnostics.EventKey(changed), diagnostics.EventKey(outside), batchKey} {
		if _, err := f.service.store.Head(ctx, key); err != nil {
			t.Fatalf("protected or out-of-range object %q was removed: %v", key, err)
		}
	}
	lateInside := diagnostics.Event{ID: "late-inside", OccurredAt: from.Add(50 * time.Minute)}
	behind, err := diagnostics.EventBehindWatermark(ctx, f.service.store, lateInside)
	if err != nil || !behind {
		t.Fatalf("selected event time sealed = %t, error=%v", behind, err)
	}
	lateOutside := diagnostics.Event{ID: "late-outside", OccurredAt: to.Add(40 * time.Minute)}
	behind, err = diagnostics.EventBehindWatermark(ctx, f.service.store, lateOutside)
	if err != nil || behind {
		t.Fatalf("adjacent event time sealed = %t, error=%v", behind, err)
	}
}

func TestHistoryCleanupSegmentCandidateLimits(t *testing.T) {
	progress := historyCleanupRangeProgress{Counts: newHistoryCleanupCounts("diagnostic_events")}
	accepted, err := appendHistoryCleanupCandidate(&progress, historyCleanupCandidateRef{Key: "one", Size: historyCleanupSegmentBytes})
	if err != nil || !accepted || progress.SegmentItems != 1 || progress.SegmentBytes != historyCleanupSegmentBytes {
		t.Fatalf("candidate at byte limit accepted=%t progress=%+v error=%v", accepted, progress, err)
	}
	accepted, err = appendHistoryCleanupCandidate(&progress, historyCleanupCandidateRef{Key: "two", Size: 1})
	if err != nil || accepted || progress.SegmentItems != 1 || progress.SegmentBytes != historyCleanupSegmentBytes {
		t.Fatalf("candidate beyond byte limit accepted=%t progress=%+v error=%v", accepted, progress, err)
	}
	oversized := historyCleanupRangeProgress{Counts: newHistoryCleanupCounts("diagnostic_events")}
	accepted, err = appendHistoryCleanupCandidate(&oversized, historyCleanupCandidateRef{Key: "large", Size: historyCleanupSegmentBytes + 1})
	if err != nil || !accepted || oversized.SegmentItems != 0 || oversized.Counts.Oversized != 1 || oversized.Counts.SkipReasons["object_exceeds_segment_budget"] != 1 {
		t.Fatalf("oversized candidate accepted=%t progress=%+v error=%v", accepted, oversized, err)
	}
}
