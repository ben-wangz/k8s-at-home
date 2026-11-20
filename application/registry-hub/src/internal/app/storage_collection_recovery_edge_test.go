package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestLegacyStorageUsageOutputStartsFencedReplacementBuild(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.inventory = &inventory.Repository{Store: f.service.store}
	ctx := context.Background()
	manager := f.service.tasks.Leases()
	owner, err := manager.Acquire(ctx, "diagnostics/storage-inventory", "legacy-usage", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, owner)
	state := newStorageCollection()
	state.Snapshot.SnapshotID = "legacy-usage-build"
	state.AttributionPhase = 2
	page, err := json.Marshal([]storageUsage{{Domain: "state", Dimension: "category", Name: "old", ObjectBytes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pageKey := usageRunPage(state.Snapshot.SnapshotID, storageUsageRun{Stage: "reduce"}, 0)
	if _, err := f.service.store.Put(ctx, pageKey, bytes.NewReader(page), int64(len(page)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	published := storageSnapshot{SnapshotID: "last-published", CompletedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := f.service.inventory.Immutable(ctx, storageInventoryPrefix+"current.json", published); err != nil {
		t.Fatal(err)
	}
	if err := f.service.checkpointStorageCollection(ctx, owner, &state); err != nil {
		t.Fatal(err)
	}
	found, err := f.service.legacyStorageUsageOutputExists(ctx, state)
	if err != nil || !found {
		t.Fatalf("legacy output was not detected: found=%t err=%v", found, err)
	}
	replacement, err := f.service.replaceStorageCollection(ctx, owner, state, "legacy_immutable_usage_output_without_checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Snapshot.SnapshotID == state.Snapshot.SnapshotID ||
		replacement.AutomaticRestarts != 1 || replacement.RecoveredFromBuildID != state.Snapshot.SnapshotID {
		t.Fatalf("unexpected replacement state: %+v", replacement)
	}
	current, _, err := inventory.Read[storageSnapshot](ctx, f.service.store, storageInventoryPrefix+"current.json")
	if err != nil || current.SnapshotID != published.SnapshotID {
		t.Fatalf("published report changed during rebuild: %+v %v", current, err)
	}
	stored, _, err := inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil || stored.Snapshot.SnapshotID != replacement.Snapshot.SnapshotID {
		t.Fatalf("replacement checkpoint not persisted: %+v %v", stored, err)
	}
}

func TestStorageUsagePendingRejectsConflictingImmutablePage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	state := newStorageCollection()
	state.Snapshot.SnapshotID = "usage-pending-mismatch"
	state.AttributionPhase = 2
	state.Snapshot.UsageVersion = 1
	state.Usage = map[string]storageUsage{"row": {Domain: "state", Dimension: "category", Name: "row", ObjectBytes: 1}}
	if err := f.service.advanceStorageLogicalUsage(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	pending := *state.UsagePending
	conflicting, err := json.Marshal([]storageUsage{{Domain: "state", Dimension: "category", Name: "other", ObjectBytes: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Put(context.Background(), pending.PageKey, bytes.NewReader(conflicting), int64(len(conflicting)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	err = f.service.publishStorageUsagePending(context.Background(), &state)
	if !errors.Is(err, errImmutableBatchMismatch) || diagnosticReason(err) != "immutable_batch_mismatch" {
		t.Fatalf("mismatch error = %v, class %q", err, diagnosticReason(err))
	}
}

func TestStorageCollectorRestoresDueTimeWithoutPollingOrAdmission(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	state := storageCollection{NextScanAt: time.Now().Add(time.Minute)}
	if err := inventory.Update(ctx, f.service.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil }); err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, "background/maintenance/storage_inventory", "due-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)
	guard := &storageStatusGuard{inventoryWriteGuard: &inventoryWriteGuard{Store: f.service.store}}
	f.service.store = guard
	f.service.collectorSlots = make(chan struct{}, 1)
	f.service.collectorSlots <- struct{}{}
	bounded, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	f.service.runMaintenanceLeader(bounded, holder, "storage_inventory", func(context.Context) (maintenanceProgress, error) {
		t.Error("scheduled collector executed before due time")
		return maintenanceProgress{}, nil
	})
	if guard.gets != 2 || guard.writes != 0 || len(f.service.collectorSlots) != 1 {
		t.Fatalf("scheduled collector polled or acquired capacity: reads=%d writes=%d", guard.gets, guard.writes)
	}
}

type shrinkingStorageList struct {
	storage.Store
	failed bool
	delay  time.Duration
}

func (s *shrinkingStorageList) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if limit >= 8 && !s.failed {
		s.failed = true
		return nil, "", storage.ErrTimeout
	}
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-timer.C:
		}
		s.delay = 0
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func TestStorageCollectorShrinksTimedOutPageAndResumesContinuation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	registry, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	ctx := context.Background()
	for n := range 32 {
		if _, err := registry.Put(ctx, fmt.Sprintf("fixture/%08d", n), strings.NewReader("x"), 1, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	source := &shrinkingStorageList{Store: registry, delay: 1800 * time.Millisecond}
	state := newStorageCollection()
	state.AttributionPhase, state.Domain = 3, 1
	for steps := 0; state.NextScanAt.IsZero(); steps++ {
		before := state.Token
		err := f.service.advanceStorageCollection(ctx, &state, source, nil)
		if errors.Is(err, storage.ErrTimeout) {
			if state.Token != before || state.ListPageSize != 4 {
				t.Fatal("timed-out LIST advanced or did not shrink")
			}
		} else if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(state)
		state = storageCollection{}
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		if steps > 500 {
			t.Fatal("resumed collector stalled")
		}
	}
	report, err := f.service.storageSnapshot(ctx, "")
	if err != nil || !source.failed || *report.Domains["registry"].Objects != 32 {
		t.Fatalf("slow recovery=%+v error=%v", report, err)
	}
}
