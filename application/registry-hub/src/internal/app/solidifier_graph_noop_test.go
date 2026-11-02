package app

import (
	"context"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestUnchangedGraphStillChecksGenerationAndRepairsProtectionReferences(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	counted := &nestedMetadataStore{Store: base, calls: map[string]int{}, writes: map[string]int{}}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counted)
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	root := f.root("noop-graph", descriptor)
	reference := descriptorRef(descriptor)
	save := func() error { return f.processor.saveManifestGraphNode(ctx, &root, reference, true, "discovered") }
	if err := save(); err != nil {
		t.Fatal(err)
	}
	counted.writes = map[string]int{}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	if len(counted.writes) != 0 {
		t.Fatalf("unchanged graph rewrote records: %v", counted.writes)
	}
	if _, done, err := f.service.records.DeleteCacheProtectionReferences(ctx, root.ID, 100); err != nil || !done {
		t.Fatalf("remove reference fixture: done=%t err=%v", done, err)
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	refs, _, err := f.service.records.ListCacheProtectionReferences(ctx, descriptor.Digest, "", 100)
	if err != nil || len(refs) != 1 {
		t.Fatalf("unchanged graph did not repair protection references: %d, %v", len(refs), err)
	}
	if _, err := f.service.advanceGeneration(ctx, root.Authority, root.Repository, descriptor.Digest, false); err != nil {
		t.Fatal(err)
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	node, err := f.service.records.GetGraphNode(ctx, root.ID, tasks.GraphManifest, descriptor.Digest)
	if err != nil || node.ContentGeneration == nil || *node.ContentGeneration != 1 {
		t.Fatalf("graph ignored fresh generation: %+v, %v", node, err)
	}
}
