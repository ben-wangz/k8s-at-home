package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestStartBlobIntentRoundCopiesDirectDemand(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("e", 64)}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, IntentFailed, "capacity_limit", "scope is too large"); err != nil {
		t.Fatal(err)
	}

	next, err := records.StartBlobIntentRound(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, next.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(demands) != 1 || demands[0].DemandID != next.ID || demands[0].RootTaskID != "" {
		t.Fatalf("new round demands = %#v, want one direct demand owned by the new round", demands)
	}
}

func TestGraphRecordsKeepRoleEdgesAndObservationIdentity(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	rootID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	manifest := GraphNode{RootTaskID: rootID, Role: GraphManifest, Digest: digest, MediaType: "application/vnd.oci.image.manifest.v1+json", DeclaredSize: 222, Generation: 4}
	blob := GraphNode{RootTaskID: rootID, Role: GraphBlob, Digest: digest, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", DeclaredSize: 111, Generation: 4}
	if err := records.SaveGraphNode(ctx, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := records.SaveGraphNode(ctx, &blob); err != nil {
		t.Fatal(err)
	}
	loadedManifest, err := records.GetGraphNode(ctx, rootID, GraphManifest, digest)
	if err != nil || loadedManifest.DeclaredSize != 222 {
		t.Fatalf("manifest node = %#v err=%v", loadedManifest, err)
	}
	loadedBlob, err := records.GetGraphNode(ctx, rootID, GraphBlob, digest)
	if err != nil || loadedBlob.DeclaredSize != 111 {
		t.Fatalf("blob node = %#v err=%v", loadedBlob, err)
	}

	edge := DependencyEdge{
		RootTaskID: rootID, ParentRole: GraphManifest, ParentDigest: digest, Order: 2,
		ChildRole: GraphBlob, ChildDigest: "sha256:" + strings.Repeat("c", 64),
		Descriptor: GraphEntry{
			Digest: "sha256:" + strings.Repeat("c", 64), MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 4096,
			Annotations: map[string]string{"org.opencontainers.image.title": "layer.tar"},
			Platform:    json.RawMessage(`{"architecture":"arm64","os":"linux"}`), URLs: []string{"https://example.invalid/layer"},
		},
	}
	if err := records.SaveDependencyEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	if err := records.SaveDependencyEdge(ctx, edge); err != nil {
		t.Fatalf("idempotent edge save: %v", err)
	}
	edges, next, err := records.ListDependencyEdges(ctx, rootID, "", 10)
	if err != nil || next != "" || len(edges) != 1 {
		t.Fatalf("edges = %d next=%q err=%v", len(edges), next, err)
	}
	if edges[0].Order != 2 || edges[0].Descriptor.Annotations["org.opencontainers.image.title"] != "layer.tar" || string(edges[0].Descriptor.Platform) != `{"architecture":"arm64","os":"linux"}` || len(edges[0].Descriptor.URLs) != 1 {
		t.Fatalf("edge descriptor attributes were lost: %#v", edges[0])
	}

	observation := LocalObservation{
		Authority: "registry.example", Repository: "team/image", Role: GraphBlob, Digest: edge.ChildDigest,
		ActualSize: 4096, Generation: 4, GCEpoch: 9, OperationID: fmt.Sprintf("%032x", 1), VerificationBasis: "digest_and_size",
	}
	if err := records.RecordLocalObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err := records.RecordLocalObservation(ctx, observation); err != nil {
		t.Fatalf("idempotent observation: %v", err)
	}
	conflict := observation
	conflict.ActualSize++
	if err := records.RecordLocalObservation(ctx, conflict); err == nil {
		t.Fatal("conflicting operation observation was accepted")
	}
}

func TestLocalObservationLookupRequiresCurrentGenerationAndEpoch(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	observation := LocalObservation{
		Authority: "registry.example", Repository: "team/image", Role: GraphBlob,
		Digest: "sha256:" + strings.Repeat("d", 64), ActualSize: 4096,
		Generation: 4, GCEpoch: 9, OperationID: fmt.Sprintf("%032x", 1),
		VerificationBasis: "distribution_readback",
	}
	if err := records.RecordLocalObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	loaded, err := records.FindLocalObservation(ctx, observation.Authority, observation.Repository, observation.Role, observation.Digest, observation.ActualSize, observation.Generation, observation.GCEpoch)
	if err != nil || loaded.OperationID != observation.OperationID {
		t.Fatalf("matching observation = %#v err=%v", loaded, err)
	}
	if _, err := records.FindLocalObservation(ctx, observation.Authority, observation.Repository, observation.Role, observation.Digest, observation.ActualSize, observation.Generation+1, observation.GCEpoch); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("changed content generation error = %v, want not found", err)
	}

	newEpoch := observation
	newEpoch.GCEpoch++
	newEpoch.OperationID = fmt.Sprintf("%032x", 2)
	if err := records.RecordLocalObservation(ctx, newEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := records.FindLocalObservation(ctx, observation.Authority, observation.Repository, observation.Role, observation.Digest, observation.ActualSize, observation.Generation, observation.GCEpoch); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale GC epoch error = %v, want not found", err)
	}
	loaded, err = records.FindLocalObservation(ctx, newEpoch.Authority, newEpoch.Repository, newEpoch.Role, newEpoch.Digest, newEpoch.ActualSize, newEpoch.Generation, newEpoch.GCEpoch)
	if err != nil || loaded.OperationID != newEpoch.OperationID {
		t.Fatalf("current observation = %#v err=%v", loaded, err)
	}
}
