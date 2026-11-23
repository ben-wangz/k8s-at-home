package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

type diagnosticStorageIOCounter struct {
	storage.Store
	gets  atomic.Int64
	puts  atomic.Int64
	lists atomic.Int64
}

func (s *diagnosticStorageIOCounter) Get(ctx context.Context, key string) (storage.Object, error) {
	s.gets.Add(1)
	return s.Store.Get(ctx, key)
}

func (s *diagnosticStorageIOCounter) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.puts.Add(1)
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *diagnosticStorageIOCounter) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.lists.Add(1)
	return s.Store.List(ctx, prefix, token, limit)
}

func TestDiagnosticSourceLeaseRecordsStayBoundedAsSourcesRotate(t *testing.T) {
	counter := &diagnosticStorageIOCounter{Store: newFilesystemStore(t)}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counter)
	ctx := context.Background()
	now := time.Now().UTC()
	shardKeys := make(map[string]struct{})
	for index := 0; index < 1000; index++ {
		source := workqueue.Key("notify", fmt.Sprintf("rotating-source-%03d", index))
		shardKey := diagnosticSourceLeaseShardKey(source)
		shardKeys[shardKey] = struct{}{}
		if !strings.HasPrefix(shardKey, "diagnostics/event-outbox-shards/") || len(shardKey) != len("diagnostics/event-outbox-shards/")+2 {
			t.Fatalf("source lease shard key is outside its bounded namespace: %q", shardKey)
		}
		if index >= 4 {
			continue
		}
		event := diagnostics.Event{ID: diagnostics.ID("event", source), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}
		if err := f.service.collectDiagnosticOutboxVersion(ctx, source, "source-v1", []diagnostics.Event{event}); err != nil {
			t.Fatalf("collect source %d: %v", index, err)
		}
	}
	if len(shardKeys) > 256 {
		t.Fatalf("rotating source lease keyspace has %d shards, want at most 256", len(shardKeys))
	}
	legacy, _, err := storage.Scan(ctx, f.service.store, "leases/diagnostics/event-outbox/", "", 400)
	if err != nil || len(legacy) != 0 {
		t.Fatalf("persistent per-source lease records = %d, error = %v; want none", len(legacy), err)
	}
	shards, _, err := storage.Scan(ctx, f.service.store, "leases/diagnostics/event-outbox-shards/", "", 300)
	if err != nil || len(shards) == 0 || len(shards) > 256 {
		t.Fatalf("persistent source lease shards = %d, error = %v; want 1..256", len(shards), err)
	}
	t.Logf("rotating source storage requests: LIST=%d GET=%d PUT=%d", counter.lists.Load(), counter.gets.Load(), counter.puts.Load())
	if counter.lists.Load() == 0 || counter.gets.Load() == 0 || counter.puts.Load() == 0 {
		t.Fatal("rotating source profile did not record LIST, GET, and PUT requests")
	}
	left, right := findDiagnosticSourceShardCollision()
	if diagnosticSourceLeaseShardKey(left) != diagnosticSourceLeaseShardKey(right) {
		t.Fatal("test sources do not share a source lease shard")
	}
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, diagnosticSourceLeaseShardKey(left), "test-holder", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	event := diagnostics.Event{ID: diagnostics.ID("event", right), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}
	if err := f.service.collectDiagnosticOutboxVersion(ctx, right, "source-v1", []diagnostics.Event{event}); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("collection under a held shared shard = %v, want condition failure", err)
	}
	if err := manager.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticRetentionDeletesOnlyReleasedLegacySourceLeases(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	key := diagnosticSourceLeaseKey("tasks/state/legacy-retention-test")
	holder, err := manager.Acquire(ctx, key, "legacy-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Head(ctx, "leases/"+key)
	if err != nil {
		t.Fatal(err)
	}
	var progress sourceSweepCheckpoint
	if err := f.service.cleanupReleasedDiagnosticSourceLease(ctx, entry, &progress); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, "leases/"+key); err != nil {
		t.Fatalf("active source lease was deleted: %v", err)
	}
	if err := manager.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
	entry, err = f.service.store.Head(ctx, "leases/"+key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupReleasedDiagnosticSourceLease(ctx, entry, &progress); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, "leases/"+key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("released source lease remains after cleanup: %v", err)
	}
}

func findDiagnosticSourceShardCollision() (string, string) {
	seen := make(map[string]string)
	for index := 0; ; index++ {
		source := fmt.Sprintf("tasks/state/shard-collision-%d", index)
		shard := diagnosticSourceLeaseShardKey(source)
		if previous, ok := seen[shard]; ok {
			return previous, source
		}
		seen[shard] = source
	}
}
