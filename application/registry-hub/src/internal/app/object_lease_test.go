package app

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestObjectLeaseUsesSixtySecondTTLAndTwentySecondRenewal(t *testing.T) {
	if objectLeaseTTL != 60*time.Second || objectLeaseRenewInterval != 20*time.Second {
		t.Fatalf("object lease timing = TTL %s renewal %s", objectLeaseTTL, objectLeaseRenewInterval)
	}
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := lease.NewManager(store)
	ctx := context.Background()

	digest := "sha256:" + strings.Repeat("a", 64)
	_, release, err := acquireContentLease(ctx, manager, "ghcr.io", "team/image", digest, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	key := "leases/objects/" + objectLeaseKey("ghcr.io", "team/image", digest)
	remaining := readLeaseExpiry(t, store, key).Sub(time.Now())
	if remaining < 55*time.Second || remaining > objectLeaseTTL {
		t.Fatalf("content lease TTL remaining = %s, want close to 60s", remaining)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}

	current, err := manager.Acquire(ctx, "objects/renewal", "renewal-owner", objectLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	initialExpiry := readLeaseExpiry(t, store, current.Key)
	renewCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go renewLeaseWithInterval(renewCtx, cancel, manager, current, time.Millisecond, renewed)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !readLeaseExpiry(t, store, current.Key).After(initialExpiry) {
		time.Sleep(time.Millisecond)
	}
	renewedExpiry := readLeaseExpiry(t, store, current.Key)
	if !renewedExpiry.After(initialExpiry) || renewedExpiry.Sub(time.Now()) < 55*time.Second {
		cancel()
		<-renewed
		t.Fatalf("renewed lease expiry = %s, initial = %s", renewedExpiry, initialExpiry)
	}
	cancel()
	if err := <-renewed; err != nil {
		t.Fatalf("renewal stopped with error: %v", err)
	}
	if err := manager.Release(ctx, current); err != nil {
		t.Fatal(err)
	}
}

func readLeaseExpiry(t *testing.T, store storage.Store, key string) time.Time {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.ExpiresAt
}
