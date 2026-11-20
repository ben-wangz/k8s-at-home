package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestStorageCollectorAdmissionIsIndependentAndCapped(t *testing.T) {
	s := &Service{backgroundSlots: make(chan struct{}, 2), collectorSlots: make(chan struct{}, 1)}
	s.backgroundSlots <- struct{}{}
	s.backgroundSlots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unit, release, ok := s.beginBackgroundUnit(ctx, "storage_inventory", time.Second)
	if !ok || unit.Err() != nil || len(s.backgroundSlots) != 2 || len(s.collectorSlots) != 1 {
		t.Fatal("collector starved behind ordinary admission")
	}
	blocked, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, _, admitted := s.beginBackgroundUnit(blocked, "storage_inventory", time.Second); admitted {
		t.Fatal("collector exceeded its one-slot bound")
	}
	<-s.backgroundSlots
	_, ordinaryRelease, admitted := s.beginBackgroundUnit(ctx, "ordinary", time.Second)
	if !admitted {
		t.Fatal("collector blocked ordinary progress")
	}
	ordinaryRelease()
	release()
	if len(s.collectorSlots) != 0 {
		t.Fatal("collector leaked admission on completion")
	}
}

func TestStorageCollectionCheckpointFencesStaleAndAdoptsCommittedTransition(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	owner, err := f.service.tasks.Leases().Acquire(ctx, "diagnostics/storage-inventory", "checkpoint-fence", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, owner)
	state := newStorageCollection()
	if err := f.service.checkpointStorageCollection(ctx, owner, &state); err != nil {
		t.Fatal(err)
	}
	previous, _, err := inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	first, stale := previous, previous
	first.BudgetYields++
	first.LastYieldReason = "first_transition"
	stale.BudgetYields += 2
	stale.LastYieldReason = "stale_transition"
	if err := f.service.checkpointStorageCollectionTransition(ctx, owner, &first, before); err != nil {
		t.Fatal(err)
	}
	if err := f.service.checkpointStorageCollectionTransition(ctx, owner, &stale, before); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("stale checkpoint error = %v, want condition failure", err)
	}
	if err := f.service.checkpointStorageCollectionTransition(ctx, owner, &first, before); err != nil {
		t.Fatalf("replay committed transition: %v", err)
	}
	stored, _, err := inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil || stored.BudgetYields != first.BudgetYields || stored.LastYieldReason != first.LastYieldReason {
		t.Fatalf("stored checkpoint = %+v, error %v", stored, err)
	}
}

func TestStorageUsagePendingBatchResumesFromExactPayload(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manager := f.service.tasks.Leases()
	owner, err := manager.Acquire(ctx, "diagnostics/storage-inventory", "usage-pending", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, owner)

	state := newStorageCollection()
	state.Snapshot.SnapshotID = "usage-pending-replay"
	state.AttributionPhase = 2
	state.Usage = map[string]storageUsage{}
	cursorEntries := make([]storage.Entry, 0, 3)
	for n, declaredBytes := range []int64{64, 128, 192} {
		key := storageAssociationKey(state.Snapshot.SnapshotID, "registry", fmt.Sprintf("identity-%d", n))
		data, err := json.Marshal(storageAssociationSet{
			Items:         []storageAssociation{{ImageID: fmt.Sprintf("image-%d", n), Authority: "example.org", Repository: "repo"}},
			DeclaredBytes: declaredBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := f.service.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
		if err != nil {
			t.Fatal(err)
		}
		cursorEntries = append(cursorEntries, entry)
	}
	state.AttributionUsageCursor = inventory.Cursor{Entries: cursorEntries}
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error {
		return manager.Validate(ctx, owner)
	})
	if err := f.service.advanceStorageLogicalUsage(guarded, &state); err != nil {
		t.Fatal(err)
	}
	if state.UsagePending == nil || state.AttributionUsageCursor.Entities != 0 || state.UsageRuns != 0 {
		t.Fatalf("batch was not staged before its output: %+v", state)
	}
	pending := *state.UsagePending
	staged, _, err := inventory.Read[json.RawMessage](ctx, f.service.store, pending.PayloadKey)
	if err != nil || storageUsageDigest(staged) != pending.PayloadSHA256 {
		t.Fatalf("staged payload digest does not match pending record: %v", err)
	}
	if err := f.service.checkpointStorageCollection(guarded, owner, &state); err != nil {
		t.Fatal(err)
	}
	restarted, _, err := inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.UsagePending == nil || restarted.UsagePending.BatchID != pending.BatchID {
		t.Fatalf("pending batch was not durable: %+v", restarted.UsagePending)
	}

	changed, err := json.Marshal(storageAssociationSet{
		Items:         []storageAssociation{{ImageID: "image-0", Authority: "example.org", Repository: "repo"}},
		DeclaredBytes: 999,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := cursorEntries[0]
	if _, err := f.service.store.Put(ctx, old.Key, bytes.NewReader(changed), int64(len(changed)), storage.PutOptions{IfMatch: old.Version}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.advanceStorageLogicalUsage(guarded, &restarted); err != nil {
		t.Fatal(err)
	}
	output, _, err := inventory.Read[json.RawMessage](ctx, f.service.store, pending.PageKey)
	if err != nil || !bytes.Equal(output, staged) {
		t.Fatalf("published page differs from the prepared payload: %v", err)
	}
	manifest, _, err := inventory.Read[storageUsageRunMeta](ctx, f.service.store, pending.ManifestKey)
	if err != nil || manifest != pending.Manifest {
		t.Fatalf("published manifest = %+v, want %+v, error %v", manifest, pending.Manifest, err)
	}
	if restarted.UsagePending != nil || restarted.UsageRuns != 1 || !restarted.AttributionUsageCursor.Done {
		t.Fatalf("batch progress was not committed: %+v", restarted)
	}
}
