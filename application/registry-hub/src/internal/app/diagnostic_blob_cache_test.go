package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/tasks"
	"testing"
)

func TestBlobDependencyInspectsContentIdentityAndDescriptorSize(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	object := f.cacheObject([]byte("aliased config"), "application/octet-stream")
	root := tasks.Task{ID: "diagnostic", Authority: "ghcr.io", Repository: "team/image"}
	edge := tasks.DependencyEdge{ChildRole: tasks.GraphBlob, ChildDigest: object.Digest, Descriptor: tasks.GraphEntry{Digest: object.Digest, MediaType: "application/vnd.oci.image.config.v1+json", Size: object.Size}}
	for _, media := range []string{"application/vnd.oci.image.config.v1+json", "application/vnd.oci.image.layer.v1.tar+gzip"} {
		edge.Descriptor.MediaType = media
		view := f.service.taskDependency(ctx, root, edge)
		if view.CacheState != "complete" || view.CacheMediaType != "application/octet-stream" {
			t.Fatalf("aliased blob: %+v", view)
		}
	}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: object.Digest}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent.MediaType, intent.CacheVersion = "application/octet-stream", `"older-etag"`
	if err := f.service.records.SaveBlobIntent(ctx, &intent); err != nil {
		t.Fatal(err)
	}
	view := f.service.taskDependency(ctx, root, edge)
	if view.CacheState != "complete" || view.IntentCacheVersion != `"older-etag"` || view.CacheVersion == view.IntentCacheVersion {
		t.Fatalf("newer cache rejected: %+v", view)
	}
	edge.Descriptor.Size++
	if view := f.service.taskDependency(ctx, root, edge); view.CacheState != "unknown" || view.CacheReason != "cache_descriptor_size_mismatch" {
		t.Fatalf("conflicting size: %+v", view)
	}
	edge.Descriptor.Size--
	edge.ChildRole = tasks.GraphManifest
	if view := f.service.taskDependency(ctx, root, edge); view.CacheState != "missing" {
		t.Fatalf("manifest used a different representation: %+v", view)
	}
}

func TestMissingBlobBytesDeduplicateWithinDependencyPage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	configuration := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("not cached")), MediaType: "application/vnd.oci.image.config.v1+json", Size: 10}
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configuration, Layers: []artifact.Descriptor{configuration}})
	root := f.root("missing-alias", manifest)
	_ = f.step(root.ID)
	response := diagnosticGet(t, f.service, "/api/v1/tasks/"+root.ID+"/dependencies")
	var page dependencyPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.BlockingInPage != 2 || page.KnownMissingBytesInPage != 10 {
		t.Fatalf("missing bytes: %d %s", response.Code, response.Body.String())
	}
}
