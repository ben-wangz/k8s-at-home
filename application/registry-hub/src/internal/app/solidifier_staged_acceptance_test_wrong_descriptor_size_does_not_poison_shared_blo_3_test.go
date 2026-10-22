package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestWrongDescriptorSizeDoesNotPoisonSharedBlobForAnotherRoot(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	shared := f.cacheObject([]byte("shared configuration"), "application/octet-stream")
	shared.MediaType = "application/vnd.oci.image.config.v1+json"
	wrong := shared
	wrong.Size++
	bad := f.root("bad-size", f.cacheManifest(artifact.Manifest{SchemaVersion: 2,
		MediaType: ociManifestMediaType, Config: &wrong, Layers: []artifact.Descriptor{}}))
	err := f.step(bad.ID)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "manifest_invalid" {
		t.Fatalf("wrong edge result = %v, want permanent manifest_invalid", err)
	}
	goodDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2,
		MediaType: ociManifestMediaType, Config: &shared, Layers: []artifact.Descriptor{}})
	good := f.root("correct-size", goodDescriptor)
	var yielded *tasks.Yield
	if err := f.step(good.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("valid root with shared bytes = %v, want waiting for independent transfer", err)
	}
	f.transfer(shared)
	if err := f.step(good.ID); err != nil {
		t.Fatalf("valid root after unrelated size failure: %v", err)
	}
	if _, ok := f.published("bad-size"); ok {
		t.Fatal("invalid root was published")
	}
	if object, ok := f.published("correct-size"); !ok || artifact.CalculateSHA256(object.body) != goodDescriptor.Digest {
		t.Fatal("unrelated wrong-size edge poisoned the valid root")
	}
}

func TestReusedLocalBlobAndMissingLayerDoNotCompleteAbandonedPull(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	localBody := []byte("configuration already in Distribution")
	local := artifact.Descriptor{Digest: artifact.CalculateSHA256(localBody), Size: int64(len(localBody)),
		MediaType: "application/vnd.oci.image.config.v1+json"}
	f.objects[local.Digest] = stagedAcceptanceObject{body: localBody, mediaType: "application/octet-stream"}
	missing := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("client never requested this layer")),
		Size: 33, MediaType: "application/vnd.oci.image.layer.v1.tar"}
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &local, Layers: []artifact.Descriptor{missing}})
	root := f.root("abandoned-pull", descriptor)
	for range 2 {
		var yielded *tasks.Yield
		if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
			t.Fatalf("incomplete client pull = %v, want dependency wait", err)
		}
	}
	for _, reference := range []string{descriptor.Digest, "abandoned-pull"} {
		if _, ok := f.published(reference); ok {
			t.Fatalf("incomplete manifest %s was published", reference)
		}
	}
	if len(f.service.solidificationSlots) != 0 || len(f.uploads) != 0 {
		t.Fatal("unissued layer request caused background payload work or retained a worker")
	}
	current, err := f.service.tasks.Get(context.Background(), root.ID)
	if err != nil || current.Status != tasks.WaitingDeps {
		t.Fatalf("abandoned root = %#v, %v", current, err)
	}
}

func TestRootlessVerifiedBlobProtectionCanBeCancelledOrExpired(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("expire=%t", expire), func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("direct blob without a manifest"), "application/octet-stream")
			intent, err := f.service.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
				Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest,
			})
			if err != nil {
				t.Fatal(err)
			}
			cached, err := f.service.cache.GetAnyRetained(ctx, intent.Identity.Authority, intent.Identity.Repository, blob.Digest)
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.activateBlobIntent(ctx, intent, cached)
			_ = cached.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.transfer(blob)
			if state := f.service.gate.State(); state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != blob.Size {
				t.Fatalf("rootless verified blob protection = %#v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC before rootless termination = %v", err)
			}
			if expire {
				expired, err := f.service.tasks.CancelIfExpired(ctx, intent.ID, time.Now().Add(25*time.Hour))
				if err != nil || !expired {
					t.Fatalf("expire rootless verified task = %t, %v", expired, err)
				}
			} else if _, err := f.service.tasks.Cancel(ctx, intent.ID); err != nil {
				t.Fatal(err)
			}
			task, err := f.service.tasks.Get(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.processor.TaskTerminal(ctx, &task); err != nil {
				t.Fatal(err)
			}
			if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
				t.Fatalf("rootless safe termination retained protection = %#v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); err != nil {
				t.Fatalf("GC after rootless safe termination: %v", err)
			}
		})
	}
}

func TestClientReaderSurvivesBlobTransferAndCleanupBeforeManifestPublication(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	body := []byte("client response remains open while background transfer finishes")
	blob := f.cacheObject(body, "application/octet-stream")
	reader, err := f.service.cache.GetAnyRetained(ctx, "ghcr.io", "team/image", blob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Body.Close()
	prefix := make([]byte, 7)
	if _, err := io.ReadFull(reader.Body, prefix); err != nil {
		t.Fatal(err)
	}
	blob.MediaType = "application/vnd.oci.image.config.v1+json"
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root("pinned-reader", descriptor)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("prepare root = %v", err)
	}
	f.transfer(blob)
	if _, err := f.service.records.FindLocalObservation(ctx, root.Authority, root.Repository, tasks.GraphBlob,
		blob.Digest, blob.Size, 0, f.service.gate.State().Epoch); err != nil {
		t.Fatalf("durable verification before cleanup: %v", err)
	}
	if _, ok := f.published(descriptor.Digest); ok {
		t.Fatal("manifest published before the explicit finalization step")
	}
	if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
		t.Fatalf("cleanup with active client reader = %d, %v", removed, err)
	}
	rest, err := io.ReadAll(reader.Body)
	if err != nil || !bytes.Equal(append(prefix, rest...), body) {
		t.Fatalf("client response after background transfer = %q, %v", rest, err)
	}
	if err := reader.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 1 {
		t.Fatalf("cleanup after reader close but before manifest publication = %d, %v", removed, err)
	}
	if f.service.gate.State().ProtectionSessionCount != 1 {
		t.Fatal("blob cache was removed without continuous unanchored protection")
	}
	if err := f.step(root.ID); err != nil {
		t.Fatalf("publish manifest after independent blob cleanup: %v", err)
	}
}
