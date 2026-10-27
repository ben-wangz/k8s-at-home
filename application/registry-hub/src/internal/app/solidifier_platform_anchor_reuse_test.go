package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"testing"
	"time"
)

const testOCIConfigMediaType = "application/vnd.oci.image.config.v1+json"

func publishAnchoredTestPlatform(f *stagedAcceptanceFixture, name string) (artifact.Descriptor, artifact.Descriptor, []byte) {
	f.t.Helper()
	blobBody := []byte(name + " platform configuration")
	blob := f.cacheObject(blobBody, testOCIConfigMediaType)
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root(name, manifest)
	assertWaitingDeps(f.t, f.step(root.ID))
	f.transfer(blob)
	if err := f.step(root.ID); err != nil {
		f.t.Fatalf("publish platform %s: %v", name, err)
	}
	return manifest, blob, blobBody
}

func addColdTestPlatform(f *stagedAcceptanceFixture, name, architecture string) (artifact.Descriptor, []byte) {
	f.t.Helper()
	blobBody := []byte(name + " cold configuration")
	blob := artifact.Descriptor{Digest: artifact.CalculateSHA256(blobBody), Size: int64(len(blobBody)), MediaType: testOCIConfigMediaType}
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
	manifest.Platform = []byte(`{"os":"linux","architecture":"` + architecture + `"}`)
	return manifest, blobBody
}

func addMultiPlatformParent(f *stagedAcceptanceFixture, reference string, platforms ...artifact.Descriptor) tasks.Task {
	f.t.Helper()
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &platforms})
	return f.root(reference, rootDescriptor)
}

func assertWaitingDeps(t *testing.T, err error) {
	t.Helper()
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("processing result = %v, want waiting_dependencies", err)
	}
}

func recacheTestObject(f *stagedAcceptanceFixture, descriptor artifact.Descriptor, body []byte) {
	f.t.Helper()
	if err := f.service.cache.Put(context.Background(), "ghcr.io", "team/image", descriptor.Digest,
		descriptor.MediaType, bytes.NewReader(body), descriptor.Size); err != nil {
		f.t.Fatal(err)
	}
}

func TestAnchoredPlatformReuseFinalizesGraphAndCleansCacheBeforeOtherPlatformArrives(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	platformManifest, configBlob, configBody := publishAnchoredTestPlatform(f, "linux-amd64")
	manifestObject, ok := f.published(platformManifest.Digest)
	if !ok {
		t.Fatal("published platform manifest is missing from Distribution")
	}
	recacheTestObject(f, platformManifest, manifestObject.body)
	recacheTestObject(f, configBlob, configBody)
	coldManifest, _ := addColdTestPlatform(f, "linux-arm64", "arm64")
	parent := addMultiPlatformParent(f, "multi", platformManifest, coldManifest)
	assertWaitingDeps(t, f.step(parent.ID))

	ctx := context.Background()
	manifestNode, err := f.service.records.GetGraphNode(ctx, parent.ID, tasks.GraphManifest, platformManifest.Digest)
	if err != nil || manifestNode.LocalState != "anchored" {
		t.Fatalf("reused manifest graph node = %#v, %v; want anchored", manifestNode, err)
	}
	blobNode, err := f.service.records.GetGraphNode(ctx, parent.ID, tasks.GraphBlob, configBlob.Digest)
	if err != nil || blobNode.LocalState != "anchored" {
		t.Fatalf("reused blob graph node = %#v, %v; want anchored", blobNode, err)
	}
	if task, err := f.service.tasks.Get(ctx, parent.ID); err != nil || task.Status != tasks.WaitingDeps {
		t.Fatalf("multi-platform parent task = %#v, %v; want to remain waiting", task, err)
	}
	f.service.cache.SetOrphanProtection(f.service.protectCacheObject)
	f.service.cacheProtectionReady.Store(true)
	removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || removed < 2 {
		t.Fatalf("cleanup after anchored reuse = %d, %v; want at least manifest and config cache removed", removed, err)
	}
	for _, digest := range []string{platformManifest.Digest, configBlob.Digest} {
		if _, err := f.service.cache.GetAnyRetained(ctx, parent.Authority, parent.Repository, digest); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("reused cache digest %s remains after cleanup: %v", digest, err)
		}
	}
	if _, err := f.service.cache.GetAnyRetained(ctx, parent.Authority, parent.Repository, coldManifest.Digest); err != nil {
		t.Fatalf("incomplete platform manifest cache was removed: %v", err)
	}

	manifestNode.LocalState = "verified_present"
	if err := f.service.records.SaveGraphNode(ctx, &manifestNode); err != nil {
		t.Fatal(err)
	}
	cursor := anchoredManifestReuseCursor{}
	for page := 0; page < 4; page++ {
		next, done, err := f.service.reconcileAnchoredManifestReusePage(ctx, cursor)
		if err != nil {
			t.Fatalf("reconcile stale anchored manifest node: %v", err)
		}
		cursor = next
		if done {
			break
		}
	}
	if task, err := f.service.tasks.Get(ctx, parent.ID); err != nil || task.Status != tasks.Pending {
		t.Fatalf("reconciled parent task = %#v, %v; want pending", task, err)
	}
}

func TestAnchoredPlatformReuseHonorsOtherRootsAndRemainsReadableAfterCleanup(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	platformManifest, configBlob, configBody := publishAnchoredTestPlatform(f, "linux-amd64-shared")
	manifestObject, ok := f.published(platformManifest.Digest)
	if !ok {
		t.Fatal("published platform manifest is missing from Distribution")
	}
	recacheTestObject(f, platformManifest, manifestObject.body)
	recacheTestObject(f, configBlob, configBody)
	coldManifestA, _ := addColdTestPlatform(f, "linux-arm64-shared-a", "arm64")
	coldManifestB, _ := addColdTestPlatform(f, "linux-arm64-shared-b", "arm64")
	parentA := addMultiPlatformParent(f, "multi-shared-a", platformManifest, coldManifestA)
	parentB := addMultiPlatformParent(f, "multi-shared-b", platformManifest, coldManifestB)
	assertWaitingDeps(t, f.step(parentA.ID))
	assertWaitingDeps(t, f.step(parentB.ID))

	ctx := context.Background()
	manifestNode, err := f.service.records.GetGraphNode(ctx, parentB.ID, tasks.GraphManifest, platformManifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	manifestNode.LocalState = "verified_present"
	if err := f.service.records.SaveGraphNode(ctx, &manifestNode); err != nil {
		t.Fatal(err)
	}
	blobNode, err := f.service.records.GetGraphNode(ctx, parentB.ID, tasks.GraphBlob, configBlob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	blobNode.LocalState = "discovered"
	if err := f.service.records.SaveGraphNode(ctx, &blobNode); err != nil {
		t.Fatal(err)
	}
	f.service.cache.SetOrphanProtection(f.service.protectCacheObject)
	f.service.cacheProtectionReady.Store(true)
	if _, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil {
		t.Fatalf("cleanup while another root has incomplete anchored state: %v", err)
	}
	for _, digest := range []string{platformManifest.Digest, configBlob.Digest} {
		if _, err := f.service.cache.GetAnyRetained(ctx, parentB.Authority, parentB.Repository, digest); err != nil {
			t.Fatalf("shared digest %s was removed while another root still protected it: %v", digest, err)
		}
	}

	cursor := anchoredManifestReuseCursor{}
	for page := 0; page < 4; page++ {
		next, done, err := f.service.reconcileAnchoredManifestReusePage(ctx, cursor)
		if err != nil {
			t.Fatalf("reconcile stale shared manifest node: %v", err)
		}
		cursor = next
		if done {
			break
		}
	}
	if task, err := f.service.tasks.Get(ctx, parentB.ID); err != nil || task.Status != tasks.Pending {
		t.Fatalf("reconciled second parent task = %#v, %v; want pending", task, err)
	}
	assertWaitingDeps(t, f.step(parentB.ID))

	removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(10*time.Minute), 100)
	if err != nil || removed < 2 {
		t.Fatalf("cleanup after all active roots anchor shared content = %d, %v; want manifest and config removed", removed, err)
	}
	for _, digest := range []string{platformManifest.Digest, configBlob.Digest} {
		if _, err := f.service.cache.GetAnyRetained(ctx, parentB.Authority, parentB.Repository, digest); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("shared cache digest %s remains after cleanup: %v", digest, err)
		}
	}

	if _, verifiedManifest, err := f.processor.readDistributionManifest(ctx, &parentB, contentRef{
		digest: platformManifest.Digest, mediaType: platformManifest.MediaType, size: platformManifest.Size,
	}); err != nil || verifiedManifest.digest != platformManifest.Digest {
		t.Fatalf("read anchored manifest from Distribution after cache cleanup = %#v, %v", verifiedManifest, err)
	}
	blobPath := "/v2/" + f.processor.internalName(parentB.Authority, parentB.Repository) + "/blobs/" + configBlob.Digest
	if err := f.processor.verifyLocalObject(ctx, blobPath, configBlob.Digest, configBlob.Size); err != nil {
		t.Fatalf("read anchored blob from Distribution after cache cleanup: %v", err)
	}
}

func TestAnchoredPlatformReuseDoesNotFinalizeWhenDependencyIsMissing(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	platformManifest, configBlob, configBody := publishAnchoredTestPlatform(f, "linux-amd64-missing")
	manifestObject, ok := f.published(platformManifest.Digest)
	if !ok {
		t.Fatal("published platform manifest is missing from Distribution")
	}
	recacheTestObject(f, platformManifest, manifestObject.body)
	recacheTestObject(f, configBlob, configBody)
	coldManifest, _ := addColdTestPlatform(f, "linux-arm64-missing", "arm64")
	parent := addMultiPlatformParent(f, "multi-missing", platformManifest, coldManifest)
	f.mu.Lock()
	delete(f.objects, configBlob.Digest)
	f.mu.Unlock()
	assertWaitingDeps(t, f.step(parent.ID))

	node, err := f.service.records.GetGraphNode(context.Background(), parent.ID, tasks.GraphManifest, platformManifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if node.LocalState == "anchored" {
		t.Fatal("manifest was anchored while one of its Distribution blobs was missing")
	}
	if _, err := f.service.cache.GetAnyRetained(context.Background(), parent.Authority, parent.Repository, configBlob.Digest); err != nil {
		t.Fatalf("cache copy of missing Distribution blob was not retained: %v", err)
	}
	_, _, err = f.service.reconcileAnchoredManifestReusePage(context.Background(), anchoredManifestReuseCursor{})
	if err != nil {
		t.Fatalf("reconcile missing Distribution dependency: %v", err)
	}
	if task, err := f.service.tasks.Get(context.Background(), parent.ID); err != nil || task.Status != tasks.WaitingDeps {
		t.Fatalf("missing-dependency parent task = %#v, %v; want to remain waiting", task, err)
	}
}

func TestAnchoredPlatformReuseStopsOnGCEpochChange(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	platformManifest, configBlob, configBody := publishAnchoredTestPlatform(f, "linux-amd64-gc")
	manifestObject, ok := f.published(platformManifest.Digest)
	if !ok {
		t.Fatal("published platform manifest is missing from Distribution")
	}
	recacheTestObject(f, platformManifest, manifestObject.body)
	recacheTestObject(f, configBlob, configBody)
	coldManifest, _ := addColdTestPlatform(f, "linux-arm64-gc", "arm64")
	parent := addMultiPlatformParent(f, "multi-gc", platformManifest, coldManifest)
	var once sync.Once
	drainResult := make(chan error, 1)
	f.beforeRequest = func(request *http.Request) {
		if request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/blobs/"+configBlob.Digest) {
			once.Do(func() { drainResult <- f.service.gate.BeginDrain(context.Background()) })
		}
	}
	err := f.step(parent.ID)
	assertWaitingDeps(t, err)
	if err := <-drainResult; err != nil {
		t.Fatalf("begin GC drain during reuse verification: %v", err)
	}
	if task, err := f.service.tasks.Get(context.Background(), parent.ID); err != nil || task.WaitReason != "gc_admission" {
		t.Fatalf("GC epoch-change wait state = %#v, %v; want gc_admission", task, err)
	}
	node, err := f.service.records.GetGraphNode(context.Background(), parent.ID, tasks.GraphManifest, platformManifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if node.LocalState == "anchored" {
		t.Fatal("manifest was anchored across a GC epoch change")
	}
}
