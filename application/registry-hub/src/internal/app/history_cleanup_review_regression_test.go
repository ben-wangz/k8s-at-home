package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestHistoryCleanupTransferRangeSkipsAbsentAndOutOfRangeObservations(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-60 * 24 * time.Hour)
	writeTransferHistoryFixture(t, s, transferView{
		ID: diagnostics.ID("recent-active"), Kind: "client_upload", TransferState: "in_progress",
		StartedAt: now, ProgressAt: now, ObservedAt: now,
	})
	req := historyCleanupRangeRequest{Category: "transfer_observations", From: from, To: from.Add(time.Hour)}
	empty := completeHistoryRangePreview(t, s, req)
	if empty.Counts.Protected != 0 || empty.Counts.Eligible != 0 {
		t.Fatalf("recent active transfer counted in an old range: %+v", empty.Counts)
	}

	preview, err := s.createHistoryCleanupRangePreview(ctx, "operator", req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := s.readHistoryCleanupRangeProgress(ctx, preview.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefix := diagnostics.Prefix + "transfer-sessions/"
	liveKey := prefix + "following.json"
	if err := putHistoryCleanupJSON(ctx, s.store, liveKey, transferView{Kind: "upstream_download", TransferState: "in_progress"}, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	at := from.Add(time.Minute).UnixNano()
	progress.PrefixIndex = 1
	progress.Cursor = inventory.Cursor{Entries: []storage.Entry{
		{Key: prefix + "deleted.json", Size: 120, ModTime: at},
		{Key: liveKey, Size: 200, ModTime: at},
	}}
	if err := s.saveHistoryCleanupRangeCheckpoint(ctx, preview.ID, progress); err != nil {
		t.Fatal(err)
	}
	if _, done, err := s.historyCleanupRangePreviewStep(ctx, &job); err != nil || !done {
		t.Fatalf("preview did not advance past the stale list entry: done=%t err=%v", done, err)
	}
	result, err := s.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
	if err != nil || result.Status != "completed" || result.Counts.Scanned != 2 ||
		result.Counts.Protected != 1 || result.Counts.SkipReasons["active_transfer"] != 1 || result.Counts.Eligible != 0 {
		t.Fatalf("preview did not preserve the next in-range observation: %+v err=%v", result, err)
	}
}

func TestDiagnosticRetentionReclaimsOnlyOrphanedReceiptArchives(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-120 * time.Hour)
	for _, name := range []string{"deleted", "retained"} {
		receipt := workqueue.Job{SchemaVersion: 1, ID: workqueue.ID("notify", name), Kind: "notify",
			CreatedAt: at.Add(-time.Minute), CompletedAt: at, State: "done", Payload: []byte("{}")}
		source := putTestReceipt(t, s, receipt)
		if name == "deleted" {
			if err := s.cleanupWorkReceipt(ctx, source, &sourceSweepCheckpoint{}, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.Head(ctx, source.Key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("receipt deletion did not complete: %v", err)
			}
		}
		key := diagnosticReceiptArchiveKey(source.Key)
		if err := inventory.Update(ctx, s.store, key, func(current *diagnosticReceiptArchive) error {
			current.UpdatedAt = at
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		entry, err := s.store.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		entry.ModTime = at.UnixNano()
		var progress sourceSweepCheckpoint
		if err := s.cleanupDiagnosticRetentionEntry(ctx, entry, &progress); err != nil {
			t.Fatal(err)
		}
		_, err = s.store.Head(ctx, key)
		if name == "retained" {
			if err != nil || progress.Status.Counts["skipped"] != 1 {
				t.Fatalf("archive needed by retained receipt: err=%v counts=%v", err, progress.Status.Counts)
			}
			continue
		}
		if !errors.Is(err, storage.ErrNotFound) || progress.Status.Counts["deleted"] != 1 {
			t.Fatalf("orphaned archive survived: err=%v counts=%v", err, progress.Status.Counts)
		}
		if _, err := s.store.Head(ctx, workqueue.CompletionMarkerKey(receipt.Kind, receipt.ID)); err != nil {
			t.Fatalf("completion marker was removed with the archive: %v", err)
		}
	}
}

func TestHistoryCleanupRetentionPaginatesAndKeepsLatestPreviewPerCategory(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 2*historyCleanupMaxPreviews + 1 {
		id := fmt.Sprintf("%064x", i+1)
		preview := historyCleanupRangePreview{ID: id, Category: "diagnostic_events", Status: "completed",
			CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
		if i == 2*historyCleanupMaxPreviews {
			preview.ExpiresAt = now.Add(time.Hour)
			oldest := now.Add(-10 * 24 * time.Hour)
			preview.Counts = historyCleanupCounts{Eligible: 3, EligibleBytes: 300, OldestEligibleAt: &oldest}
		}
		if err := putHistoryCleanupJSON(ctx, s.store, historyCleanupRangePreviewKey(id), preview, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := historyCleanupPreview{Complete: true, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Counts: map[string]historyCleanupCounts{"terminal_work_history": {Eligible: 2, EligibleBytes: 200}}}
	if err := putHistoryCleanupJSON(ctx, s.store, historyCleanupPreviewKey("latest-cutoff"), cutoff, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	view, err := s.historyCleanupRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts := view.KnownEligibleVolume["diagnostic_events"]; counts.Eligible != 3 || counts.EligibleBytes != 300 || counts.OldestEligibleAt == nil {
		t.Fatalf("live range preview on later page not represented: %+v", counts)
	}
	if counts := view.KnownEligibleVolume["terminal_work_history"]; counts.Eligible != 2 || counts.EligibleBytes != 200 {
		t.Fatalf("cutoff preview for another category not represented: %+v", counts)
	}
}
