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
