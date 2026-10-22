package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func (f *stagedAcceptanceFixture) step(id string) error {
	f.t.Helper()
	ctx := context.Background()
	task, err := f.service.tasks.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	currentLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+id, "staged-acceptance", time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, currentLease)
	task.Status, task.WaitReason = tasks.Running, ""
	if err := f.service.tasks.SaveProgress(ctx, &task, currentLease); err != nil {
		f.t.Fatal(err)
	}
	result := f.processor.Process(ctx, &task, currentLease)
	task, err = f.service.tasks.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	var yielded *tasks.Yield
	switch {
	case errors.As(result, &yielded):
		task.Status, task.WaitReason = yielded.Status, yielded.Reason
	case result == nil:
		task.Status = tasks.Solidified
	default:
		task.Status = tasks.Failed
		var failure *tasks.Failure
		if errors.As(result, &failure) && !failure.Permanent {
			task.Status = tasks.RetryWait
		}
	}
	if err := f.service.tasks.SaveProgress(ctx, &task, currentLease); err != nil {
		f.t.Fatal(err)
	}
	f.processor.TaskYielded(ctx, &task)
	return result
}

func (f *stagedAcceptanceFixture) transfer(descriptor artifact.Descriptor) {
	f.t.Helper()
	ctx := context.Background()
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: descriptor.Digest,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.step(intent.ID)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.VerifiedLocal {
		f.t.Fatalf("transfer %s = %v, want verified_local", descriptor.Digest, err)
	}
}

func (f *stagedAcceptanceFixture) published(reference string) (stagedAcceptanceObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[reference]
	return object, ok
}

func TestStagedPlatformsPublishIndependentlyWithDelayedArm64Demand(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	amd := f.cacheObject([]byte("amd64 configuration"), "application/octet-stream")
	amd.MediaType = "application/vnd.oci.image.config.v1+json"
	armBody := []byte("arm64 configuration")
	arm := artifact.Descriptor{Digest: artifact.CalculateSHA256(armBody), Size: int64(len(armBody)), MediaType: amd.MediaType}
	amdManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &amd, Layers: []artifact.Descriptor{}})
	armManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &arm, Layers: []artifact.Descriptor{}})
	amdManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"amd64"}`)
	armManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"arm64"}`)
	children := []artifact.Descriptor{amdManifest, armManifest}
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("latest", rootDescriptor)
	wantWaiting := func(err error) {
		t.Helper()
		var yielded *tasks.Yield
		if !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
			t.Fatalf("root processing = %v, want waiting_dependencies", err)
		}
	}
	wantWaiting(f.step(root.ID))
	f.transfer(amd)
	wantWaiting(f.step(root.ID))
	if _, ok := f.published(amdManifest.Digest); !ok {
		t.Fatal("ready amd64 platform was not published before arm64 arrived")
	}
	for _, reference := range []string{armManifest.Digest, rootDescriptor.Digest, "latest"} {
		if _, ok := f.published(reference); ok {
			t.Fatalf("incomplete scope %s was published", reference)
		}
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("anchored amd64 and cold arm64 still block GC: %#v", state)
	}
	if len(f.service.solidificationSlots) != 0 {
		t.Fatal("dependency wait retained a worker slot")
	}
	wantWaiting(f.step(root.ID))
	f.cacheObject(armBody, "application/octet-stream")
	wantWaiting(f.step(root.ID))
	f.transfer(arm)
	if err := f.step(root.ID); err != nil {
		t.Fatalf("finish root after arm64 arrives: %v", err)
	}
	object, ok := f.published("latest")
	if !ok || artifact.CalculateSHA256(object.body) != rootDescriptor.Digest {
		t.Fatal("completed root does not preserve the original index bytes")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads[amd.Digest] != 1 || f.uploads[arm.Digest] != 1 {
		t.Fatalf("platform blob uploads = %v, want one for each platform", f.uploads)
	}
}

func TestOversizedStagedScopeDoesNotContactDistributionOrCleanCache(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 512)
	configDescriptor := f.cacheObject(bytes.Repeat([]byte("c"), 128), "application/octet-stream")
	layers := []artifact.Descriptor{f.cacheObject(bytes.Repeat([]byte("l"), 256), "application/octet-stream")}
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &configDescriptor, Layers: layers})
	root := f.root("oversized", rootDescriptor)
	err := f.step(root.ID)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "capacity_limit" {
		t.Fatalf("oversized scope result = %v, want permanent capacity_limit", err)
	}
	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	if requests != 0 {
		t.Fatalf("oversized scope made %d Distribution calls", requests)
	}
	if removed, err := f.service.cache.Cleanup(context.Background(), time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
		t.Fatalf("cleanup after capacity rejection = %d, %v, want retained inputs", removed, err)
	}
	for _, descriptor := range append(layers, configDescriptor) {
		cached, err := f.service.cache.GetAnyRetained(context.Background(), root.Authority, root.Repository, descriptor.Digest)
		if err != nil {
			t.Fatalf("rejected scope lost cache input: %v", err)
		}
		_ = cached.Body.Close()
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("oversized scope acquired protection or quota: %#v", state)
	}
}
