package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

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
