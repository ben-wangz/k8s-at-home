package app

import (
	"bytes"
	"context"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestReferrerMigrationStateIsIdempotentAndGenerational(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := &Service{store: store}
	ctx := context.Background()
	authority := "ghcr.io"
	repository := "team/image"
	subject := "sha256:" + strings.Repeat("1", 64)
	fallbackDigest := "sha256:" + strings.Repeat("2", 64)
	nativeDigest := "sha256:" + strings.Repeat("3", 64)
	nativeDigestV2 := "sha256:" + strings.Repeat("4", 64)
	referrers := []string{"sha256:" + strings.Repeat("5", 64)}

	first, err := service.storeReferrerMigration(ctx, authority, repository, subject, fallbackDigest, nativeDigest, referrers)
	if err != nil {
		t.Fatalf("first migration state write failed: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("first generation = %d, want 1", first.Generation)
	}

	repeated, err := service.storeReferrerMigration(ctx, authority, repository, subject, fallbackDigest, nativeDigest, referrers)
	if err != nil {
		t.Fatalf("repeated migration state write failed: %v", err)
	}
	if repeated.Generation != first.Generation || repeated.UpdatedAt != first.UpdatedAt {
		t.Fatalf("repeated identical migration changed state: first=%#v repeated=%#v", first, repeated)
	}

	changed, err := service.storeReferrerMigration(ctx, authority, repository, subject, fallbackDigest, nativeDigestV2, referrers)
	if err != nil {
		t.Fatalf("changed migration state write failed: %v", err)
	}
	if changed.Generation != 2 || changed.NativeIndexDigest != nativeDigestV2 {
		t.Fatalf("changed generation = %#v, want generation 2 with new native digest", changed)
	}

	loaded, err := service.loadReferrerMigration(ctx, authority, repository, subject)
	if err != nil {
		t.Fatalf("migration state load failed: %v", err)
	}
	if loaded.Generation != changed.Generation || loaded.NativeIndexDigest != changed.NativeIndexDigest {
		t.Fatalf("loaded state = %#v, want %#v", loaded, changed)
	}

	if _, err := store.Put(ctx, referrerMigrationKey(authority, repository, subject), bytes.NewReader([]byte(`{"schemaVersion":1}`)), int64(len(`{"schemaVersion":1}`)), storage.PutOptions{}); err != nil {
		t.Fatalf("corrupt migration state write failed: %v", err)
	}
	if _, err := service.loadReferrerMigration(ctx, authority, repository, subject); err == nil {
		t.Fatal("loadReferrerMigration accepted corrupt state")
	}
}

func TestPendingReferrerMigrationStaysPendingUntilVerified(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := &Service{store: store}
	ctx := context.Background()
	authority := "ghcr.io"
	repository := "team/image"
	subject := "sha256:" + strings.Repeat("1", 64)
	fallbackDigest := "sha256:" + strings.Repeat("2", 64)
	nativeDigest := "sha256:" + strings.Repeat("3", 64)
	referrers := []string{"sha256:" + strings.Repeat("4", 64)}

	pending, err := service.storePendingReferrerMigration(ctx, authority, repository, subject, fallbackDigest, referrers)
	if err != nil {
		t.Fatalf("pending migration state write failed: %v", err)
	}
	if pending.State != referrerMigrationMigrating {
		t.Fatalf("pending state = %q, want %q", pending.State, referrerMigrationMigrating)
	}
	loaded, err := service.loadReferrerMigration(ctx, authority, repository, subject)
	if err != nil || loaded.State != referrerMigrationMigrating {
		t.Fatalf("pending migration state was not preserved: state=%q err=%v", loaded.State, err)
	}
	routed := route.Route{Upstream: authority, RemoteRepo: repository, HasUpstream: true}
	pendingResult, err := service.referrerMigrationPending(ctx, routed, subject, true)
	if err != nil || !pendingResult {
		t.Fatalf("pending migration was not treated as authoritative: pending=%t err=%v", pendingResult, err)
	}

	complete, err := service.storeReferrerMigration(ctx, authority, repository, subject, fallbackDigest, nativeDigest, referrers)
	if err != nil {
		t.Fatalf("completed migration state write failed: %v", err)
	}
	if complete.State != referrerMigrationComplete || complete.Generation != pending.Generation+1 {
		t.Fatalf("completed state = %#v, want complete generation %d", complete, pending.Generation+1)
	}
	pendingResult, err = service.referrerMigrationPending(ctx, routed, subject, true)
	if err != nil || pendingResult {
		t.Fatalf("completed migration remained pending: pending=%t err=%v", pendingResult, err)
	}
}
