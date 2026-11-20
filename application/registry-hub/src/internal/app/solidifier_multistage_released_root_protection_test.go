package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
)

func TestReusedAnchoredManifestClearsReleasedRootProtectionBeforeTagPublication(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	configBody := []byte("{}")
	config := f.cacheObject(configBody, "application/vnd.oci.image.config.v1+json")
	rootManifest := artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        &config,
		Layers:        []artifact.Descriptor{},
	}
	rootBody, err := json.Marshal(rootManifest)
	if err != nil {
		t.Fatal(err)
	}
	rootDescriptor := f.cacheObject(rootBody, ociManifestMediaType)
	root := f.root("latest", rootDescriptor)
	root.Graph[rootDescriptor.Digest] = tasks.GraphEntry{
		Digest: rootDescriptor.Digest, MediaType: rootDescriptor.MediaType, Size: rootDescriptor.Size,
	}
	root.Status = tasks.Running
	rootLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+root.ID, "released-root-session-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, rootLease)
	scopeID, err := protectionScopeID(root.Authority, root.Repository, rootDescriptor.Digest, root.Generation)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := stagedSessionID(scopeID)
	scopeBytes := rootDescriptor.Size + config.Size
	if err := f.service.gate.AdmitProtection(ctx, sessionID, scopeID, root.ID, scopeBytes, 4, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.service.gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
		t.Fatal(err)
	}
	root.SessionIDs = []string{sessionID}
	root.ScopeID = scopeID
	root.GCProtected = true
	root.RequestedBytes, root.ReservedBytes = scopeBytes, scopeBytes
	if err := f.service.tasks.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.objects[config.Digest] = stagedAcceptanceObject{body: configBody, mediaType: config.MediaType}
	f.objects[rootDescriptor.Digest] = stagedAcceptanceObject{body: rootBody, mediaType: ociManifestMediaType}
	f.mu.Unlock()
	manifest := &stagedManifest{
		ref:        contentRef{digest: rootDescriptor.Digest, mediaType: rootDescriptor.MediaType, size: rootDescriptor.Size},
		blobs:      []contentRef{{digest: config.Digest, mediaType: config.MediaType, size: config.Size}},
		scopeID:    scopeID,
		sessionID:  sessionID,
		scopeBytes: scopeBytes,
	}
	finalized, err := f.processor.finalizeReusedAnchoredManifest(ctx, &root, rootLease, manifest)
	if err != nil || !finalized {
		t.Fatalf("finalize reused anchored manifest = %t, %v", finalized, err)
	}
	persisted, err := f.service.tasks.Get(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.SessionIDs) != 0 || persisted.GCProtected || persisted.ReservedBytes != 0 {
		t.Fatalf("root task after anchored-session cleanup = %#v", persisted)
	}
	if err := f.processor.publishRootTag(ctx, &root, rootLease); err != nil {
		t.Fatalf("publish root tag after anchored reuse: %v", err)
	}
	if _, ok := f.published("latest"); !ok {
		t.Fatal("root tag was not published")
	}
}
