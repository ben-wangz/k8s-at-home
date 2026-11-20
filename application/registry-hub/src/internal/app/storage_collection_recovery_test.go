package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

type storageReplayFault struct {
	storage.Store
	target  string
	fired   bool
	unknown bool
	writes  map[string][]byte
}

func (s *storageReplayFault) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if options.IfAbsent && strings.Contains(key, "/snapshots/") {
		if previous, ok := s.writes[key]; ok && !bytes.Equal(previous, data) {
			return storage.Entry{}, fmt.Errorf("nonidentical immutable replay: %s", key)
		}
		s.writes[key] = append([]byte(nil), data...)
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), size, options)
	if err == nil && !s.fired && strings.Contains(key, s.target) {
		s.fired = true
		if s.unknown {
			return entry, storage.ErrUnknownCommit
		}
		return entry, storage.ErrUnavailable
	}
	return entry, err
}

func TestStorageCollectorReplaysDurableOutputAfterFailureAndTakeover(t *testing.T) {
	for _, target := range []string{"/objects/", "/usage/reduce/", "/usage/sort/", "/manifest.json", "/current.json", "/checkpoint.json"} {
		t.Run(target, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			registry, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			ctx := context.Background()
			for n := range 23 {
				if _, err := registry.Put(ctx, fmt.Sprintf("registry/%08d", n), strings.NewReader("payload"), 7, storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			fault := &storageReplayFault{Store: f.service.store, target: target, unknown: target == "/current.json", writes: map[string][]byte{}}
			f.service.store, f.service.inventory.Store = fault, fault
			manager := f.service.tasks.Leases()
			owner, err := manager.Acquire(ctx, "diagnostics/storage-inventory", "first", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			state := newStorageCollection()
			if err := f.service.checkpointStorageCollection(ctx, owner, &state); err != nil && !fault.fired {
				t.Fatal(err)
			}
			taken := false
			for quantum := 0; ; quantum++ {
				state, _, err = inventory.Read[storageCollection](ctx, fault, storageInventoryPrefix+"checkpoint.json")
				if err != nil {
					t.Fatal(err)
				}
				if !state.NextScanAt.IsZero() {
					break
				}
				if quantum > 1000 {
					t.Fatal("recovery did not converge")
				}
				_, err := f.service.storageCollectionQuantum(ctx, owner, &state, inspectionReadOnlyStore{registry}, nil)
				if err != nil && !errors.Is(err, storage.ErrUnavailable) {
					t.Fatal(err)
				}
				if fault.fired && !taken {
					if err := manager.Release(ctx, owner); err != nil {
						t.Fatal(err)
					}
					owner, err = manager.Acquire(ctx, "diagnostics/storage-inventory", "replacement", time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					taken = true
				}
			}
			defer manager.Release(ctx, owner)
			report, err := f.service.storageSnapshot(ctx, "")
			if err != nil || !fault.fired || *report.Domains["registry"].Objects != 23 || *report.Domains["registry"].ObjectBytes != 161 {
				t.Fatalf("replayed report=%+v fired=%t err=%v", report, fault.fired, err)
			}
			if err := manager.Release(ctx, owner); err != nil {
				t.Fatal(err)
			}
			state = newStorageCollection()
			if _, err := f.service.storageCollectionQuantum(ctx, owner, &state, registry, nil); err == nil {
				t.Fatal("lost owner committed collector work")
			}
		})
	}
}

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
