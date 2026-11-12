package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestCompletedDiscoveryReusesEdgesAndRepairsMissingProtection(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	counted := &nestedMetadataStore{Store: base, calls: map[string]int{}, writes: map[string]int{}}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counted)
	blob := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("not cached")), Size: 10, MediaType: "application/octet-stream"}
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{blob}})
	root := f.root("reuse-discovery", manifest)
	if err := f.step(root.ID); err == nil {
		t.Fatal("cold dependency unexpectedly completed")
	}
	if _, done, err := f.service.records.DeleteCacheProtectionReferences(ctx, root.ID, 100); err != nil || !done {
		t.Fatalf("remove protection: %t, %v", done, err)
	}
	counted.writes = map[string]int{}
	copy := *f.processor
	f.processor = &copy
	if err := f.step(root.ID); err == nil {
		t.Fatal("cold dependency unexpectedly completed after restart")
	}
	for key, n := range counted.writes {
		if strings.Contains(key, "/edges/") && n != 0 {
			t.Fatalf("reused discovery rewrote immutable edge %s (%d)", key, n)
		}
	}
	for _, digest := range []string{blob.Digest, manifest.Digest} {
		references, _, err := f.service.records.ListCacheProtectionReferences(ctx, digest, "", 100)
		if err != nil || len(references) != 1 {
			t.Fatalf("protection not repaired for %s: %v, %v", digest, references, err)
		}
	}
	entries, _, err := storage.Scan(ctx, f.service.store, "solidification/graphs/"+root.ID+"/edges/", "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("edges=%v, %v", entries, err)
	}
	if err := f.service.store.Delete(ctx, entries[0].Key); err != nil {
		t.Fatal(err)
	}
	_ = f.step(root.ID)
	entries, _, err = storage.Scan(ctx, f.service.store, "solidification/graphs/"+root.ID+"/edges/", "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing edge was not repaired: %v, %v", entries, err)
	}
}

func TestDiscoverySnapshotRespectsChangedLimits(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	blob := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("cold")), Size: 4, MediaType: "application/octet-stream"}
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{blob}})
	children := []artifact.Descriptor{manifest}
	index := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("changed-limits", index)
	_ = f.step(root.ID)
	f.processor.cfg.Limits.ClosureDepth = 0
	var failure *tasks.Failure
	if err := f.step(root.ID); !errors.As(err, &failure) || !failure.Permanent || failure.Class != "closure_limit" {
		t.Fatalf("changed depth limit was ignored: %v", err)
	}
}

func TestDependencySignalOnlyProcessesAffectedPlatform(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	amd := f.cacheObject([]byte("amd config"), "application/octet-stream")
	arm := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("arm config")), Size: 10, MediaType: "application/octet-stream"}
	amdManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &amd, Layers: []artifact.Descriptor{}})
	armManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &arm, Layers: []artifact.Descriptor{}})
	children := []artifact.Descriptor{amdManifest, armManifest}
	index := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("incremental", index)
	_ = f.step(root.ID)
	f.transfer(amd)
	armProbes := 0
	f.beforeRequest = func(request *http.Request) {
		if strings.HasSuffix(request.URL.Path, arm.Digest) {
			armProbes++
		}
	}
	_ = f.step(root.ID)
	if armProbes != 0 {
		t.Fatalf("unaffected arm platform probed %d times", armProbes)
	}
	if _, ok := f.published(amdManifest.Digest); !ok {
		t.Fatal("affected ready platform was not published")
	}
	if _, ok := f.published("incremental"); ok {
		t.Fatal("incomplete root was published")
	}
	execution, _, err := inventory.Read[graphExecution](context.Background(), f.service.store, graphExecutionKey(root.ID))
	if err != nil || !execution.Ready[amdManifest.Digest] || execution.Ready[armManifest.Digest] {
		t.Fatalf("incremental state=%+v, %v", execution, err)
	}
	// Lost hints use full processing and can discover newly cached data.
	f.beforeRequest = nil
	f.cacheObject([]byte("arm config"), "application/octet-stream")
	_ = f.step(root.ID)
	f.transfer(arm)
	if err := f.step(root.ID); err != nil {
		t.Fatalf("fallback did not converge: %v", err)
	}
}

func TestSharedDependencyMarksEveryAncestor(t *testing.T) {
	parents := map[string][]string{"layer": {"amd", "arm"}, "amd": {"nested"}, "arm": {"root"}, "nested": {"root"}}
	affected := make(map[string]bool)
	markAffectedManifests(parents, affected, "layer")
	for _, digest := range []string{"amd", "arm", "nested", "root"} {
		if !affected[digest] {
			t.Fatalf("ancestor %s was omitted", digest)
		}
	}
}
