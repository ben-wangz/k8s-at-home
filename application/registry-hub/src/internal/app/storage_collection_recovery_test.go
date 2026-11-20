package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/inventory"
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
