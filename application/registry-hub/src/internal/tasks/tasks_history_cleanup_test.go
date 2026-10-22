package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestTaskCleanupRemovesGraphAndRootDemandIndexesInBoundedPass(t *testing.T) {
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
	rootDigest := "sha256:" + strings.Repeat("e", 64)
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", rootDigest, "application/vnd.oci.image.index.v1+json", 2)
	if err != nil {
		t.Fatal(err)
	}
	records := NewRecords(store)
	if err := records.SaveGraphNode(ctx, &GraphNode{
		RootTaskID: task.ID, Role: GraphManifest, Digest: rootDigest,
		MediaType: "application/vnd.oci.image.index.v1+json", DeclaredSize: 100, ActualSize: 100,
		Generation: task.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, rootDigest); err != nil {
		t.Fatal(err)
	}
	intent, _, err := records.EnsureBlobIntent(ctx, BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64),
	}, task.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	demandID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if err := records.LinkBlobDemandScope(ctx, intent.ID, demandID, task.ID, "scope-1", sessionID, 100); err != nil {
		t.Fatal(err)
	}
	rootDemandIndex := rootBlobDemandIndexKey(task.ID, intent.ID, demandID)
	rootDemandIndexEntry, err := store.Head(ctx, rootDemandIndex)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIfMatch(ctx, rootDemandIndex, rootDemandIndexEntry.Version); err != nil {
		t.Fatal(err)
	}
	if err := records.RebuildRootBlobDemandIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, rootDemandIndex); err != nil {
		t.Fatalf("root demand reverse index was not rebuilt: %v", err)
	}
	task.Status = Solidified
	task.RootPublished = true
	task.UpdatedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}

	removed, err := manager.Cleanup(ctx, 24*time.Hour, 10)
	if err != nil || removed != 1 {
		t.Fatalf("Cleanup() = %d, %v; want one task removed", removed, err)
	}
	if _, err := manager.Get(ctx, task.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cleaned task lookup error = %v, want not found", err)
	}
	if _, err := records.GetGraphNode(ctx, task.ID, GraphManifest, rootDigest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cleaned graph node lookup error = %v, want not found", err)
	}
	protectionRefs, _, err := records.ListCacheProtectionReferences(ctx, rootDigest, "", 10)
	if err != nil || len(protectionRefs) != 0 {
		t.Fatalf("remaining cache protection refs = %#v, %v; want none", protectionRefs, err)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 100)
	if err != nil || len(demands) != 0 {
		t.Fatalf("remaining root blob demands = %#v, %v; want none", demands, err)
	}
}

func TestBlobTaskCleanupRemovesTerminalIntentAndIdentityIndex(t *testing.T) {
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
	records := NewRecords(store)
	identity := BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("1", 64)}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-version", 5)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AnchorBlobTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, IntentAnchored, "", ""); err != nil {
		t.Fatal(err)
	}
	task, err = manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	task.UpdatedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}

	removed, err := manager.Cleanup(ctx, 24*time.Hour, 10)
	if err != nil || removed != 1 {
		t.Fatalf("Cleanup() = %d, %v; want one blob task removed", removed, err)
	}
	if _, err := manager.Get(ctx, task.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cleaned blob task lookup error = %v, want not found", err)
	}
	if _, err := records.GetBlobIntent(ctx, intent.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cleaned intent lookup error = %v, want not found", err)
	}
	if _, err := records.GetBlobIntentByIdentity(ctx, identity); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cleaned identity index lookup error = %v, want not found", err)
	}
}
