package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestEnqueueResolvedIfIdleRejectsConflictingActiveRoot(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	mediaType := "application/vnd.oci.image.manifest.v1+json"
	firstDigest := testRootDigest()
	active, created, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", firstDigest, mediaType, 3)
	if err != nil || !created {
		t.Fatalf("EnqueueResolved() = (%#v, %t, %v); want created root", active, created, err)
	}

	conflictingDigest := "sha256:" + strings.Repeat("b", 64)
	if _, created, err := manager.EnqueueResolvedIfIdle(ctx, "ghcr.io", "team/image", "latest", conflictingDigest, mediaType, 3); !errors.Is(err, ErrActiveRootDemand) || created {
		t.Fatalf("EnqueueResolvedIfIdle() = (created %t, %v); want active-root conflict", created, err)
	}
	current, err := manager.Get(ctx, active.ID)
	if err != nil || current.Status != Pending || current.RootDigest != firstDigest {
		t.Fatalf("original active root = %#v, %v; it must remain untouched", current, err)
	}
	activeTasks, err := manager.Active(ctx)
	if err != nil || len(activeTasks) != 1 || activeTasks[0].ID != active.ID {
		t.Fatalf("active roots = %#v, %v; want only the original task", activeTasks, err)
	}
}

func TestEnqueueResolvedIfIdleJoinsMatchingActiveRootAndRequeuesTerminalRoot(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	mediaType := "application/vnd.oci.image.manifest.v1+json"
	digest := testRootDigest()
	active, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", digest, mediaType, 3)
	if err != nil {
		t.Fatal(err)
	}
	joined, created, err := manager.EnqueueResolvedIfIdle(ctx, "ghcr.io", "team/image", "latest", digest, mediaType, 3)
	if err != nil || created || joined.ID != active.ID {
		t.Fatalf("matching active root = (%#v, %t, %v); want existing task", joined, created, err)
	}

	active.Status = Solidified
	active.RootPublished = true
	active.TerminalCleanupComplete = true
	if err := manager.save(ctx, &active); err != nil {
		t.Fatal(err)
	}
	replacement, created, err := manager.EnqueueResolvedIfIdle(ctx, "ghcr.io", "team/image", "latest", digest, mediaType, 3)
	if err != nil || !created || replacement.ID == active.ID {
		t.Fatalf("terminal root replay = (%#v, %t, %v); want a new round", replacement, created, err)
	}
}
