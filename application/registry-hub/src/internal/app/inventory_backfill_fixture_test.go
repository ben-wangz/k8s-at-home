package app

import (
	"context"
	"fmt"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

type inventoryBackfillStore struct {
	storage.Store
	edgeReads         int
	delay             time.Duration
	failCheckpoint    bool
	unknownCheckpoint bool
}

func (s *inventoryBackfillStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.Contains(key, "/edges/") {
		s.edgeReads++
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return storage.Object{}, ctx.Err()
		case <-timer.C:
		}
	}
	return s.Store.Get(ctx, key)
}

func (s *inventoryBackfillStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if key == inventory.Prefix+"checkpoint.json" && s.failCheckpoint {
		s.failCheckpoint = false
		return storage.Entry{}, storage.ErrTimeout
	}
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && key == inventory.Prefix+"checkpoint.json" && s.unknownCheckpoint {
		s.unknownCheckpoint = false
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, err
}

func seedInventoryHistoricalGraphs(t *testing.T, f *stagedAcceptanceFixture) {
	t.Helper()
	ctx := context.Background()
	for root := 0; root < 20; root++ {
		manifests, nodes, edges := 5, 16, 15
		if root < 4 {
			manifests++
		}
		if root < 2 {
			nodes++
		}
		if root < 3 {
			edges++
		}
		refs := make([]artifact.Descriptor, nodes)
		for n := range refs {
			refs[n] = artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte(fmt.Sprintf("root-%d-node-%d", root, n))), MediaType: "application/octet-stream", Size: 1}
			if n < manifests {
				refs[n].MediaType = ociManifestMediaType
			}
		}
		task := f.root(fmt.Sprintf("historical-%d", root), refs[0])
		for n, ref := range refs {
			role := tasks.GraphBlob
			if n < manifests {
				role = tasks.GraphManifest
			}
			node := tasks.GraphNode{RootTaskID: task.ID, Role: role, Digest: ref.Digest, MediaType: ref.MediaType, DeclaredSize: ref.Size, ActualSize: ref.Size, DiscoveryComplete: true}
			if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
				t.Fatal(err)
			}
		}
		for e := 0; e < edges; e++ {
			parent, child := refs[e%manifests], refs[manifests+e%(nodes-manifests)]
			edge := tasks.DependencyEdge{RootTaskID: task.ID, ParentRole: tasks.GraphManifest, ParentDigest: parent.Digest, Order: e / manifests, ChildRole: tasks.GraphBlob, ChildDigest: child.Digest, Descriptor: tasks.GraphEntry{Digest: child.Digest, MediaType: child.MediaType, Size: child.Size}}
			if err := f.service.records.SaveDependencyEdge(ctx, edge); err != nil {
				t.Fatal(err)
			}
		}
	}
	for n := 0; n < 50; n++ {
		id := fmt.Sprintf("%032x", n+1)
		task := tasks.Task{ID: id, Kind: tasks.BlobKind}
		if err := inventory.Update(ctx, f.service.store, "tasks/state/"+id+".json", func(value *tasks.Task) error { *value = task; return nil }); err != nil {
			t.Fatal(err)
		}
	}
}
