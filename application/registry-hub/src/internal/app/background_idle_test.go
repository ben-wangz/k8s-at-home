package app

import (
	"context"
	"io"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type idleQueueStore struct {
	storage.Store
	polls, checkpoints atomic.Int64
}

func (s *idleQueueStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if prefix == workqueue.Prefix+"jobs/idle_test/" {
		s.polls.Add(1)
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func (s *idleQueueStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.HasPrefix(key, workqueue.Prefix+"queue-checkpoints/") {
		s.checkpoints.Add(1)
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestIdleQueueBackoffCheckpointBudgetAndMissedWake(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &idleQueueStore{Store: base}
	s := &Service{store: store, instanceID: "idle-test", backgroundReady: map[string]chan struct{}{"idle_test": make(chan struct{}, 1)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder, err := lease.NewManager(store).Acquire(ctx, "idle-test", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runBackgroundQueueLeader(ctx, holder, "idle_test", func(_ context.Context, job *workqueue.Job) (any, bool, error) {
			processed <- job.ID
			return nil, true, nil
		})
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(6 * time.Second)
	if polls, writes := store.polls.Load(), store.checkpoints.Load(); polls > 3 || polls == 0 || writes != 1 {
		t.Fatalf("idle storage budget exceeded: polls=%d checkpoint writes=%d", polls, writes)
	}
	for _, notify := range []bool{true, false} {
		q := workqueue.Queue{Store: store}
		if notify {
			q = s.backgroundQueue()
		}
		job, err := q.Enqueue(ctx, "idle_test", time.Now().String(), "payload", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		budget := 6 * time.Second
		if notify {
			budget = time.Second
		}
		select {
		case id := <-processed:
			if id != job.ID {
				t.Fatal("processed a different durable job")
			}
		case <-time.After(budget):
			t.Fatalf("durable work was lost (wake=%t)", notify)
		}
	}
}
