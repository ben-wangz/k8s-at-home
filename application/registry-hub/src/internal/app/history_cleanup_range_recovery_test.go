package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestHistoryCleanupPreviewRecoversCandidateChunkAfterCheckpointFailure(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &historyCleanupCheckpointFaultStore{Store: base}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	day := time.Now().UTC().Add(-f.service.diagnosticsConfig().EventRetention - 36*time.Hour).Truncate(24 * time.Hour)
	from, to := day.Add(2*time.Hour), day.Add(3*time.Hour)
	events := make([]diagnostics.Event, historyCleanupBatchSize)
	for index := range events {
		events[index] = diagnostics.Event{ID: fmt.Sprintf("checkpoint-recovery-%02d", index), OccurredAt: from.Add(time.Duration(index) * time.Second)}
		if err := putHistoryCleanupJSON(ctx, f.service.store, diagnostics.EventKey(events[index]), events[index], storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := f.service.createHistoryCleanupRangePreview(ctx, "operator", historyCleanupRangeRequest{Category: "diagnostic_events", From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	store.progressKey, store.failProgressWrite = historyCleanupRangeProgressKey(preview.ID), true
	job, err := f.service.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID))
	if err != nil {
		t.Fatal(err)
	}
	checkpointFailed := false
	for range 8 {
		progress, done, stepErr := f.service.historyCleanupRangePreviewStep(ctx, &job)
		if stepErr != nil {
			if errors.Is(stepErr, storage.ErrUnavailable) {
				checkpointFailed = true
				break
			}
			t.Fatal(stepErr)
		}
		job.Progress, err = json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			t.Fatal("preview completed before the injected checkpoint failure")
		}
	}
	if !checkpointFailed || !store.chunkWritten {
		t.Fatal("candidate chunk checkpoint failure was not exercised")
	}
	for _, event := range events {
		entry, err := f.service.store.Head(ctx, diagnostics.EventKey(event))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
			t.Fatal(err)
		}
	}
	completed := false
	for range 8 {
		progress, done, stepErr := f.service.historyCleanupRangePreviewStep(ctx, &job)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		job.Progress, err = json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			completed = true
			break
		}
	}
	if !completed {
		t.Fatal("preview did not resume from its persisted candidate chunk")
	}
	result, err := f.service.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.SegmentItems != len(events) || result.Counts.Eligible != int64(len(events)) {
		t.Fatalf("recovered preview lost its candidate chunk: %+v", result)
	}
}

func TestHistoryCleanupSegmentStorageReadFailuresRemainRetryable(t *testing.T) {
	for _, stage := range []string{"segment", "candidate chunk"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			store := &historyCleanupReadFaultStore{Store: base}
			f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
			operation := historyCleanupOperation{
				SchemaVersion: 1, ID: "cleanup-operation", PreviewID: "cleanup-preview", Principal: "operator",
				Categories: []string{"diagnostic_events"}, Cutoff: time.Now().UTC().Add(-time.Hour),
				Range:     historyCleanupRange{From: time.Now().UTC().Add(-48 * time.Hour), To: time.Now().UTC().Add(-47 * time.Hour)},
				SegmentID: "cleanup-segment", PolicyVersion: historyCleanupPolicyVersion,
			}
			segment := historyCleanupSegment{ID: operation.SegmentID, PreviewID: operation.PreviewID, Category: operation.Categories[0], Items: 1, Objects: 1, Bytes: 1, Chunks: 1}
			segmentKey := historyCleanupSegmentKey(operation.SegmentID)
			chunkKey := historyCleanupSegmentChunkKey(operation.SegmentID, 0)
			faultKey := segmentKey
			if stage == "candidate chunk" {
				if err := putHistoryCleanupJSON(ctx, f.service.store, segmentKey, segment, storage.PutOptions{IfAbsent: true}); err != nil {
					t.Fatal(err)
				}
				faultKey = chunkKey
			}
			store.key, store.fail = faultKey, true
			_, _, err = f.service.historyCleanupSegmentStep(ctx, ctx, operation, historyCleanupProgress{
				SegmentID: operation.SegmentID, Counts: map[string]historyCleanupCounts{"diagnostic_events": newHistoryCleanupCounts("diagnostic_events")},
			})
			if !errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrCorruptObject) {
				t.Fatalf("%s read error was misclassified: %v", stage, err)
			}
		})
	}
}

func TestFailedHistoryCleanupTasksDoNotConsumePreviewCapacity(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	now := time.Now().UTC()
	for index := range historyCleanupMaxPreviews {
		previewID := diagnostics.ID("failed-preview", fmt.Sprint(index))
		preview := historyCleanupRangePreview{
			SchemaVersion: 1, ID: previewID, Principal: "operator", Category: "diagnostic_events",
			PolicyVersion: historyCleanupPolicyVersion, Status: "failed", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := putHistoryCleanupJSON(ctx, f.service.store, historyCleanupRangePreviewKey(previewID), preview, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
		job, err := f.service.backgroundQueue().Enqueue(ctx, historyCleanupPreviewKind, previewID,
			historyCleanupRangeJobPayload{PreviewID: previewID, Principal: "operator", PolicyVersion: historyCleanupPolicyVersion}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := inventory.Update(ctx, f.service.store, workqueue.Key(historyCleanupPreviewKind, job.ID), func(current *workqueue.Job) error {
			current.State = "failed"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if index < historyCleanupMaxResults {
			operationJob, err := f.service.backgroundQueue().Enqueue(ctx, historyCleanupWorkKind, fmt.Sprintf("failed-operation-%02d", index),
				historyCleanupJobPayload{PreviewID: previewID, Principal: "operator", PolicyVersion: historyCleanupPolicyVersion}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := inventory.Update(ctx, f.service.store, workqueue.Key(historyCleanupWorkKind, operationJob.ID), func(current *workqueue.Job) error {
				current.State = "failed"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			operation := historyCleanupOperation{
				SchemaVersion: 1, ID: operationJob.ID, PreviewID: previewID, Principal: "operator",
				Categories: []string{"diagnostic_events"}, Cutoff: now.Add(-time.Hour), PolicyVersion: historyCleanupPolicyVersion,
				Status: "running", CreatedAt: now,
			}
			if err := putHistoryCleanupJSON(ctx, f.service.store, historyCleanupOperationKey(operation.ID), operation, storage.PutOptions{IfAbsent: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.service.checkHistoryCleanupRangeCapacity(ctx); err != nil {
		t.Fatalf("failed previews blocked new cleanup work: %v", err)
	}
	if err := f.service.ensureHistoryCleanupCapacity(ctx); err != nil {
		t.Fatalf("failed cleanup tasks blocked new operations: %v", err)
	}
	for index := range historyCleanupMaxResults {
		operationID := workqueue.ID(historyCleanupWorkKind, fmt.Sprintf("failed-operation-%02d", index))
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, f.service.store, historyCleanupOperationKey(operationID))
		if err != nil || operation.Status != "failed" || operation.ExpiresAt.IsZero() {
			t.Fatalf("failed operation was not terminalized: operation=%+v error=%v", operation, err)
		}
	}
}

type historyCleanupCheckpointFaultStore struct {
	storage.Store
	progressKey       string
	failProgressWrite bool
	chunkWritten      bool
}

func (store *historyCleanupCheckpointFaultStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.Contains(key, "/chunks/") {
		store.chunkWritten = true
	}
	if key == store.progressKey && store.chunkWritten && store.failProgressWrite {
		store.failProgressWrite = false
		return storage.Entry{}, storage.ErrUnavailable
	}
	return store.Store.Put(ctx, key, reader, size, options)
}

type historyCleanupReadFaultStore struct {
	storage.Store
	key  string
	fail bool
}

func (store *historyCleanupReadFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if store.fail && key == store.key {
		store.fail = false
		return storage.Object{}, storage.ErrUnavailable
	}
	return store.Store.Get(ctx, key)
}
