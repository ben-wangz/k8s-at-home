package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestLeaseReadMigratesPreviousSchemaWithoutChangingFence(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := NewManager(store)
	ctx := context.Background()
	active, err := manager.Acquire(ctx, "objects/shared", "owner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	object, err := store.Get(ctx, active.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, object.Body); err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	previous := record{
		SchemaVersion: leaseSchemaPrevious,
		Owner:         active.Owner,
		FencingToken:  active.FencingToken,
		AcquiredAt:    time.Now().UTC(),
		ExpiresAt:     active.ExpiresAt,
		State:         "held",
	}
	data, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, active.Key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version}); err != nil {
		t.Fatal(err)
	}

	if err := manager.Validate(ctx, active); err != nil {
		t.Fatalf("validate migrated lease: %v", err)
	}
	migrated, err := store.Get(ctx, active.Key)
	if err != nil {
		t.Fatal(err)
	}
	var persisted record
	if err := json.NewDecoder(migrated.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	_ = migrated.Body.Close()
	if persisted.SchemaVersion != leaseSchemaCurrent || persisted.FencingToken != active.FencingToken {
		t.Fatalf("migrated lease = %#v, want current schema and unchanged fencing token %d", persisted, active.FencingToken)
	}
	if _, err := manager.Acquire(ctx, "objects/shared", "owner-b", time.Minute); !errors.Is(err, ErrHeld) {
		t.Fatalf("acquire while migrated lease is active = %v, want ErrHeld", err)
	}
}

func TestLeaseRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := NewManager(store)
	ctx := context.Background()
	active, err := manager.Acquire(ctx, "objects/shared", "owner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Get(ctx, active.Key)
	if err != nil {
		t.Fatal(err)
	}
	var persisted record
	if err := json.NewDecoder(object.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	persisted.SchemaVersion = 99
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	unsupported, err := store.Put(ctx, active.Key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Validate(ctx, active); err == nil {
		t.Fatal("lease with an unsupported schema was accepted")
	}
	after, err := store.Head(ctx, active.Key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != unsupported.Version {
		t.Fatal("lease with an unsupported schema was rewritten")
	}
}

func TestExpiredLeaseAllowsOnlyOneFencedSuccessor(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := NewManager(store)
	ctx := context.Background()
	previous, err := manager.Acquire(ctx, "objects/shared", "previous", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(previous.ExpiresAt); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}

	type result struct {
		lease Lease
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, owner := range []string{"successor-a", "successor-b"} {
		wait.Add(1)
		go func(owner string) {
			defer wait.Done()
			<-start
			current, acquireErr := manager.Acquire(ctx, "objects/shared", owner, time.Minute)
			results <- result{lease: current, err: acquireErr}
		}(owner)
	}
	close(start)
	wait.Wait()
	close(results)

	var successor Lease
	acquired := 0
	for outcome := range results {
		if outcome.err == nil {
			successor = outcome.lease
			acquired++
		} else if !errors.Is(outcome.err, ErrHeld) {
			t.Fatalf("successor acquisition error = %v, want lease-held", outcome.err)
		}
	}
	if acquired != 1 {
		t.Fatalf("successors acquired = %d, want exactly one", acquired)
	}
	defer manager.Release(ctx, successor)
	if successor.FencingToken != previous.FencingToken+1 {
		t.Fatalf("successor fencing token = %d, want %d", successor.FencingToken, previous.FencingToken+1)
	}
	if err := manager.Validate(ctx, previous); !errors.Is(err, ErrLost) {
		t.Fatalf("previous leader validation error = %v, want ErrLost", err)
	}
	if _, err := manager.Renew(ctx, previous, time.Minute); !errors.Is(err, ErrLost) {
		t.Fatalf("previous leader renewal error = %v, want ErrLost", err)
	}
}
