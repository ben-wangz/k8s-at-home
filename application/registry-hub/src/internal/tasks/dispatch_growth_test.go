package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type dispatchGrowthStore struct {
	*filesystem.Store
	target     string
	historical atomic.Int64
	scans      atomic.Int64
	candidates atomic.Int64
}

func (s *dispatchGrowthStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, "tasks/state/") && key != s.target {
		s.historical.Add(1)
	}
	return s.Store.Get(ctx, key)
}

func (s *dispatchGrowthStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if prefix == "tasks/state/" {
		s.scans.Add(1)
	}
	if strings.HasPrefix(prefix, dispatchPrefix) {
		s.candidates.Add(1)
	}
	return s.Store.ListMetadata(ctx, prefix, token, limit)
}

func TestDispatchMeasuresPrecedingHistoryAtEveryPosition(t *testing.T) {
	for _, history := range []int{16, 256, 1024} {
		for _, position := range []int{0, history / 2, history} {
			t.Run(fmt.Sprintf("history=%d/preceding=%d", history, position), func(t *testing.T) {
				measureDispatchHistory(t, history, position)
			})
		}
	}
}

func TestDispatchMeasuresColdIndexRebuild(t *testing.T) {
	for _, history := range []int{16, 256, 1024} {
		t.Run(fmt.Sprintf("history=%d", history), func(t *testing.T) {
			raw, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			ctx, cancel := context.WithCancel(context.Background())
			target := fmt.Sprintf("%032x", history+1)
			store := &dispatchGrowthStore{Store: raw, target: taskKey(target)}
			processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
			manager, err := NewManager(store, 1, 10, processor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); manager.Close() }()
			for index := 1; index <= history+1; index++ {
				id := fmt.Sprintf("%032x", index)
				task := Task{SchemaVersion: taskSchemaCurrent, ID: id, RoundID: id, Kind: BlobKind, Status: Solidified, Phase: "solidification"}
				if id == target {
					task.Status = Pending
				}
				body, _ := json.Marshal(task)
				if _, err := raw.Put(ctx, taskKey(id), bytes.NewReader(body), int64(len(body)), storage.PutOptions{IfAbsent: true}); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			for count := 0; count < (history/16)+3; count++ {
				if err := manager.repairDispatchIndex(ctx, 0); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case task := <-processor.entered:
				if task.ID != target {
					t.Fatalf("rebuilt candidate is for %s, want %s", task.ID, target)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("legacy candidate was not rebuilt")
			}
			reads := store.historical.Load()
			if reads != int64(history) {
				t.Fatalf("rebuild historical reads = %d, want %d", reads, history)
			}
			t.Logf("history=%d rebuild_historical_gets=%d state_scans=%d rebuild_seconds=%.6f", history, reads, store.scans.Load(), time.Since(started).Seconds())
		})
	}
}

func measureDispatchHistory(t *testing.T, history, preceding int) {
	t.Helper()
	raw, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := fmt.Sprintf("%032x", preceding+1)
	store := &dispatchGrowthStore{Store: raw, target: taskKey(target)}
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); manager.Close() }()
	for index := 1; index <= history+1; index++ {
		id := fmt.Sprintf("%032x", index)
		task := Task{SchemaVersion: taskSchemaCurrent, ID: id, RoundID: id, Kind: BlobKind, Status: Solidified, Phase: "solidification"}
		if id == target {
			task.Status = Pending
		}
		body, _ := json.Marshal(task)
		if _, err := raw.Put(ctx, taskKey(id), bytes.NewReader(body), int64(len(body)), storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	runnable, err := manager.Get(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.save(ctx, &runnable); err != nil {
		t.Fatal(err)
	}
	store.historical.Store(0)
	store.scans.Store(0)
	store.candidates.Store(0)
	started := time.Now()
	manager.dispatch(ctx, 0)
	select {
	case task := <-processor.entered:
		if task.ID != target {
			t.Fatalf("dispatched %s instead of target %s", task.ID, target)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runnable task was not dispatched")
	}
	reads, scans, pages := store.historical.Load(), store.scans.Load(), store.candidates.Load()
	if reads != 0 || scans != 0 || pages != 2 {
		t.Fatalf("indexed dispatch counts: history gets=%d scans=%d candidate pages=%d preceding=%d", reads, scans, pages, preceding)
	}
	t.Logf("history=%d preceding=%d historical_gets=%d history_pages=%d candidate_pages=%d dispatch_seconds=%.6f", history, preceding, reads, scans, pages, time.Since(started).Seconds())
}
