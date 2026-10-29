package app

import (
	"context"
	"encoding/json"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"testing"
)

func TestInventoryGraphRestartsChangedNodesAndResumesHighFanout(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.Limits.MaxDescriptors = 300
	ctx := context.Background()
	digest := artifact.CalculateSHA256([]byte("high-fanout-root"))
	root := f.root("high-fanout", artifact.Descriptor{Digest: digest, Size: 1, MediaType: ociManifestMediaType})
	node := tasks.GraphNode{RootTaskID: root.ID, Role: tasks.GraphManifest, Digest: digest, MediaType: ociManifestMediaType, DeclaredSize: 1, ActualSize: 1}
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 207; n++ {
		child := artifact.CalculateSHA256([]byte(fmt.Sprintf("child-%d", n)))
		edge := tasks.DependencyEdge{RootTaskID: root.ID, ParentRole: tasks.GraphManifest, ParentDigest: digest, Order: n, ChildRole: tasks.GraphBlob, ChildDigest: child, Descriptor: tasks.GraphEntry{Digest: child, MediaType: "application/octet-stream", Size: int64(n + 1)}}
		if err := f.service.records.SaveDependencyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.service.newInventoryProjection(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed, restarted := false, false
	prefix := p.Prefix
	for n := 0; !p.Complete && n < 5000; n++ {
		if p.Nodes.Done && !changed {
			node.DiscoveryComplete = true
			if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
		if err := f.service.advanceInventoryProjection(ctx, p); err != nil {
			t.Fatal(err)
		}
		if p.Prefix != prefix {
			restarted = true
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		p = &inventoryGraphProjection{}
		if err := json.Unmarshal(data, p); err != nil {
			t.Fatal(err)
		}
	}
	if !p.Complete || !changed || !restarted {
		t.Fatalf("projection failed to resume/revalidate: complete=%v changed=%v restarted=%v", p.Complete, changed, restarted)
	}
	content, err := f.service.inventory.Content(ctx, inventoryIdentity(root.Authority, root.Repository, ""), digest)
	if err != nil || !content.ClosureKnown || len(content.Children) != 207 {
		t.Fatalf("children=%d complete=%v err=%v", len(content.Children), content.ClosureKnown, err)
	}
	for n, child := range content.Children {
		if child.Size != int64(n+1) {
			t.Fatalf("descriptor order lost at %d: %+v", n, child)
		}
	}
}

func TestInventoryGraphReplaysCapturedNodeAfterUncommittedCursor(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	digest := artifact.CalculateSHA256([]byte("changed-captured-root"))
	root := f.root("changed-capture", artifact.Descriptor{Digest: digest, Size: 1, MediaType: ociManifestMediaType})
	node := tasks.GraphNode{RootTaskID: root.ID, Role: tasks.GraphManifest, Digest: digest, MediaType: ociManifestMediaType, DeclaredSize: 1, ActualSize: 1}
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.newInventoryProjection(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.advanceInventoryProjection(ctx, p); err != nil {
		t.Fatal(err)
	}
	committed, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.advanceInventoryProjection(ctx, p); err != nil {
		t.Fatal(err)
	}
	node.DiscoveryComplete = true
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	p = &inventoryGraphProjection{}
	if err := json.Unmarshal(committed, p); err != nil {
		t.Fatal(err)
	}
	for n := 0; !p.Complete && n < 100; n++ {
		if err := f.service.advanceInventoryProjection(ctx, p); err != nil {
			t.Fatalf("replaying captured node: %v", err)
		}
	}
	if !p.Complete || p.Restarts != 1 {
		t.Fatalf("projection did not recover: complete=%v restarts=%d", p.Complete, p.Restarts)
	}
	content, err := f.service.inventory.Content(ctx, inventoryIdentity(root.Authority, root.Repository, ""), digest)
	if err != nil || !content.ClosureKnown {
		t.Fatalf("recovered closure=%v err=%v", content.ClosureKnown, err)
	}
}
