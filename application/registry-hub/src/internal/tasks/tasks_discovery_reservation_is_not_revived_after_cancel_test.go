package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestDiscoveryResolutionDoesNotReviveCancelledOrSupersededReservation(t *testing.T) {
	ctx := context.Background()
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

	first, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ResolveDiscovery(ctx, first.ID, "ghcr.io", "team/image", "latest", testRootDigest(), "application/vnd.oci.image.manifest.v1+json", 1); !errors.Is(err, ErrCancelled) {
		t.Fatalf("resolve cancelled reservation error = %v, want ErrCancelled", err)
	}

	second, created, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.ID == first.ID {
		t.Fatalf("new reservation = %#v, created=%t; want a distinct task", second, created)
	}
	if _, _, err := manager.ResolveDiscovery(ctx, first.ID, "ghcr.io", "team/image", "latest", testRootDigest(), "application/vnd.oci.image.manifest.v1+json", 1); !errors.Is(err, ErrCancelled) {
		t.Fatalf("resolve superseded reservation error = %v, want ErrCancelled", err)
	}
	current, err := manager.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.RootDigest != "" || current.Phase != "discovery" || current.Status != Pending {
		t.Fatalf("new reservation was modified by stale resolution: %#v", current)
	}
}
