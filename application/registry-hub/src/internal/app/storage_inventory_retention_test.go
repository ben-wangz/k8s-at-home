package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func seedStorageRetentionReport(t *testing.T, s *Service, expired bool) (storageSnapshot, string) {
	t.Helper()
	report := storageSnapshot{SnapshotID: inventory.ID("retention-report"), ExpiresAt: time.Now().Add(time.Hour), UsageVersion: 1, UsageRun: storageUsageRun{Stage: "sort", Level: 2}}
	if expired {
		report.ExpiresAt = time.Now().Add(-time.Hour)
	}
	prefix := storageInventoryPrefix + "snapshots/" + report.SnapshotID + "/"
	if err := inventory.Update(context.Background(), s.store, prefix+"manifest.json", func(value *storageSnapshot) error { *value = report; return nil }); err != nil {
		t.Fatal(err)
	}
	for n := range 70 {
		if err := inventory.Update(context.Background(), s.store, fmt.Sprintf("%sobjects/%08d.json", prefix, n), func(value *string) error { *value = "page"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return report, prefix
}

func TestStorageRetentionDeletesManifestLastAcrossRestart(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	report, prefix := seedStorageRetentionReport(t, f.service, true)
	manifest, err := f.service.store.Head(ctx, prefix+"manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupStorageObservation(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, manifest.Key); err != nil {
		t.Fatal("manifest disappeared before all children were deleted")
	}
	// A fresh service resumes only durable cleanup state.
	restarted := &Service{store: f.service.store}
	for step := range 10 {
		if err := restarted.advanceStorageRetention(ctx, report.SnapshotID); err != nil {
			t.Fatal(err)
		}
		_, err := restarted.store.Head(ctx, storageRetentionPrefix+report.SnapshotID+".json")
		if errors.Is(err, storage.ErrNotFound) {
			break
		}
		if step == 9 {
			t.Fatal("cleanup failed to converge")
		}
	}
	entries, _, err := storage.Scan(ctx, restarted.store, prefix, "", 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired pages survived cleanup: count=%d error=%v", len(entries), err)
	}
}

func TestStorageRetentionPreservesActiveAndFinalUsage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	report, prefix := seedStorageRetentionReport(t, f.service, false)
	final := usageRunPage(report.SnapshotID, report.UsageRun, 0)
	intermediate := usageRunPage(report.SnapshotID, storageUsageRun{Stage: "reduce"}, 0)
	for _, key := range []string{final, intermediate} {
		if err := inventory.Update(ctx, f.service.store, key, func(value *string) error { *value = "usage"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := storage.Scan(ctx, f.service.store, prefix, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := f.service.cleanupStorageObservation(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.store.Head(ctx, final); err != nil {
		t.Fatal("final usage page was deleted")
	}
	if _, err := f.service.store.Head(ctx, intermediate); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("intermediate usage page remains: %v", err)
	}
	if err := inventory.Update(ctx, f.service.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { value.Snapshot = report; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, f.service.store, prefix+"manifest.json", func(value *storageSnapshot) error { value.ExpiresAt = time.Now().Add(-time.Hour); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Key, prefix+"objects/") {
			if err := f.service.cleanupStorageObservation(ctx, entry); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.store.Head(ctx, entry.Key); err != nil {
				t.Fatal("active collection page was deleted")
			}
			break
		}
	}
}

func TestStorageRetentionRejectsReplacedManifest(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	report, prefix := seedStorageRetentionReport(t, f.service, true)
	entry, err := f.service.store.Head(ctx, prefix+"manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupStorageObservation(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, f.service.store, entry.Key, func(value *storageSnapshot) error { value.ExpiresAt = time.Now().Add(time.Hour); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.service.advanceStorageRetention(ctx, report.SnapshotID); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("replaced manifest accepted by stale deletion plan: %v", err)
	}
	if _, err := f.service.store.Head(ctx, fmt.Sprintf("%sobjects/%08d.json", prefix, 69)); err != nil {
		t.Fatal("replacement report lost its remaining page")
	}
}

func TestStorageRetentionKeepsExpiryEvidenceAfterManifestDisappears(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	report, prefix := seedStorageRetentionReport(t, f.service, true)
	entry, err := f.service.store.Head(ctx, prefix+"manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupStorageObservation(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err := f.service.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := f.service.advanceStorageRetention(ctx, report.SnapshotID); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := storage.Scan(ctx, f.service.store, prefix, "", 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("known expired output received an orphan grace: %d %v", len(entries), err)
	}
	key := storageInventoryPrefix + "snapshots/" + inventory.ID("unfinished") + "/objects/00000000.json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *string) error { *value = "unfinished"; return nil }); err != nil {
		t.Fatal(err)
	}
	orphan, err := f.service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupStorageObservation(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, key); err != nil {
		t.Fatal("young unfinished output lost its grace")
	}
	orphan.ModTime = time.Now().Add(-25 * time.Hour).UnixNano()
	if err := f.service.cleanupStorageObservation(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("expired unknown orphan was not deleted")
	}
}
