package tasks

import (
	"context"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
	"testing"
	"time"
)

type missingListedObjectStore struct {
	storage.Store
	prefix string
	used   bool
}

func (s *missingListedObjectStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if !s.used && strings.HasPrefix(key, s.prefix) {
		s.used = true
		return storage.Object{}, storage.ErrNotFound
	}
	return s.Store.Get(ctx, key)
}

func TestDemandListsIgnoreEntriesDeletedAfterListing(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	records := NewRecords(store)
	intent, _, err := records.EnsureBlobIntent(ctx, BlobIdentity{
		Authority:  "registry.example",
		Repository: "team/image",
		Digest:     "sha256:" + strings.Repeat("a", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	rootID := strings.Repeat("b", 32)
	demandID := strings.Repeat("c", 32)
	if err := records.LinkBlobDemandScope(ctx, intent.ID, demandID, rootID, "scope-1", strings.Repeat("d", 32), 64); err != nil {
		t.Fatal(err)
	}

	rootRecords := NewRecords(&missingListedObjectStore{Store: store, prefix: rootBlobDemandPrefix(rootID)})
	rootDemands, next, err := rootRecords.ListRootBlobDemandRecords(ctx, rootID, "", 100)
	if err != nil || next != "" || len(rootDemands) != 0 {
		t.Fatalf("ListRootBlobDemandRecords() = %d, %q, %v; want an empty page", len(rootDemands), next, err)
	}

	ownerPrefix := "solidification/blobs/owners/" + intent.ID + "/"
	ownerRecords := NewRecords(&missingListedObjectStore{Store: store, prefix: ownerPrefix})
	ownerDemands, next, err := ownerRecords.ListBlobDemandRecords(ctx, intent.ID, "", 100)
	if err != nil || next != "" || len(ownerDemands) != 0 {
		t.Fatalf("ListBlobDemandRecords() = %d, %q, %v; want an empty page", len(ownerDemands), next, err)
	}
}

func TestBlobIntentDeduplicatesConcurrentDemandsAndStartsNewRoundAfterCancellation(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64)}
	const demands = 12
	type result struct {
		intent  BlobIntent
		created bool
		err     error
	}
	results := make(chan result, demands)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range demands {
		workers.Add(1)
		go func() {
			defer workers.Done()
			demandID, idErr := randomID()
			if idErr != nil {
				results <- result{err: idErr}
				return
			}
			<-start
			intent, created, ensureErr := records.EnsureBlobIntent(ctx, identity, 7, demandID)
			results <- result{intent: intent, created: created, err: ensureErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var first BlobIntent
	createdCount := 0
	for item := range results {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if first.ID == "" {
			first = item.intent
		}
		if item.intent.ID != first.ID || item.intent.Round != 1 || item.intent.Generation != 7 {
			t.Fatalf("concurrent intent = %#v, want shared first round %q", item.intent, first.ID)
		}
		if item.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want exactly one", createdCount)
	}
	linked, token, err := records.ListBlobDemands(ctx, first.ID, "", 100)
	if err != nil || token != "" || len(linked) != demands {
		t.Fatalf("linked demands = %d token=%q err=%v, want %d links", len(linked), token, err, demands)
	}
	first, err = records.GetBlobIntent(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}

	first.State = IntentCancelled
	first.CancelRequested = true
	first.RetryCount = 3
	first.NextAttemptAt = time.Now().UTC()
	if err := records.SaveBlobIntent(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second, created, err := records.EnsureBlobIntent(ctx, identity, 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.ID == first.ID || second.Round != 2 || second.State != IntentWaitingCache || second.RetryCount != 0 || second.CancelRequested {
		t.Fatalf("new intent round = %#v created=%t, want a fresh second round", second, created)
	}
	loaded, err := records.GetBlobIntent(ctx, first.ID)
	if err != nil || loaded.State != IntentCancelled || loaded.RetryCount != 3 || loaded.NextAttemptAt.IsZero() {
		t.Fatalf("old round after replacement = %#v err=%v", loaded, err)
	}
}

func TestVerifiedBlobIntentStartsNewRoundWhenCacheVersionChanges(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("d", 64)}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 128)
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentState(ctx, intent.ID, IntentVerified, "", "")
	if err != nil {
		t.Fatal(err)
	}

	current, err := records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v2", 128)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID == intent.ID || current.Round != intent.Round+1 || current.State != IntentReady || current.CacheVersion != "cache-v2" {
		t.Fatalf("new cache intent = %#v, want ready next round", current)
	}
	previous, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || previous.State != IntentVerified || previous.CacheVersion != "cache-v1" {
		t.Fatalf("previous cache intent = %#v, err=%v; want preserved verified round", previous, err)
	}
}
