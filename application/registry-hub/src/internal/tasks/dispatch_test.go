package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type dispatchHistoryStore struct {
	*filesystem.Store
	historyReads atomic.Int32
	fullLists    atomic.Int32
	scans        atomic.Int32
	blockScan    chan struct{}
	entered      chan struct{}
}

func (s *dispatchHistoryStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, "tasks/state/") && key != taskKey(fmt.Sprintf("%032x", 1)) {
		s.historyReads.Add(1)
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *dispatchHistoryStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.fullLists.Add(1)
	return s.Store.List(ctx, prefix, token, limit)
}

func (s *dispatchHistoryStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.scans.Add(1)
	if s.blockScan != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		select {
		case <-s.blockScan:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return s.Store.ListMetadata(ctx, prefix, token, limit)
}

type dispatchBlockingProcessor struct{ entered chan Task }

func (p *dispatchBlockingProcessor) Process(ctx context.Context, task *Task, _ lease.Lease) error {
	p.entered <- *task
	<-ctx.Done()
	return ctx.Err()
}

func TestDispatchStartsRunnableTaskBeforeReadingUnrelatedHistory(t *testing.T) {
	for _, history := range []int{16, 256} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			raw, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &dispatchHistoryStore{Store: raw}
			processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
			manager, err := NewManager(store, 1, 10, processor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); manager.Close() }()
			for index := 1; index <= history+1; index++ {
				id := fmt.Sprintf("%032x", index)
				task := Task{SchemaVersion: taskSchemaCurrent, ID: id, RoundID: id, Kind: BlobKind, Status: Solidified, Phase: "solidification"}
				if index == 1 {
					task.Status = Pending
				}
				data, _ := json.Marshal(task)
				if _, err := raw.Put(ctx, taskKey(id), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
					t.Fatal(err)
				}
			}
			runnable, err := manager.Get(ctx, fmt.Sprintf("%032x", 1))
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.save(ctx, &runnable); err != nil {
				t.Fatal(err)
			}
			store.scans.Store(0)
			manager.dispatch(ctx, 0)
			select {
			case <-processor.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("runnable task waited for unrelated history before dispatch")
			}
			if store.historyReads.Load() != 0 || store.fullLists.Load() != 0 || store.scans.Load() != 2 {
				t.Fatalf("dispatch amplification: historical reads=%d versioned lists=%d metadata scans=%d", store.historyReads.Load(), store.fullLists.Load(), store.scans.Load())
			}
		})
	}
}

func TestConcurrentDispatchUsesOneHistoryScan(t *testing.T) {
	raw, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	store := &dispatchHistoryStore{Store: raw, blockScan: make(chan struct{}), entered: make(chan struct{}, 1)}
	manager, err := NewManager(store, 4, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); manager.dispatch(context.Background(), 0) }()
	defer func() { close(store.blockScan); <-done; manager.Close() }()
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not start its scan")
	}
	manager.dispatch(context.Background(), 1)
	if store.scans.Load() != 1 {
		t.Fatalf("concurrent workers duplicated the history scan: %d", store.scans.Load())
	}
}
