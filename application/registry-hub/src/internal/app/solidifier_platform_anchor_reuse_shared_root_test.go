package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

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
