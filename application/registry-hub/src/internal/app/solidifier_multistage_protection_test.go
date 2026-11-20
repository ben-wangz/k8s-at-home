package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestManifestAdmissionTransfersDirectBlobSessionAtContentGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	records := tasks.NewRecords(store)
	service := &Service{store: store, gate: gate, records: records, tasks: manager}
	processor := &solidifier{service: service, tasks: manager, gate: gate}
	root, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	rootLease, err := manager.Leases().Acquire(ctx, "tasks/"+root.ID, "manifest-transfer-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, rootLease)
	root.Status, root.AttemptID = tasks.Running, "root-attempt"
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	blobDigest := "sha256:" + strings.Repeat("d", 64)
	contentGeneration, err := service.advanceGeneration(ctx, root.Authority, root.Repository, blobDigest, false)
	if err != nil {
		t.Fatal(err)
	}
	if contentGeneration.Generation == root.Generation {
		t.Fatalf("test setup did not separate generations: root=%d blob=%d", root.Generation, contentGeneration.Generation)
	}
	intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: blobDigest}, contentGeneration.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	directScope, err := protectionScopeID(root.Authority, root.Repository, blobDigest, contentGeneration.Generation)
	if err != nil {
		t.Fatal(err)
	}
	oldSessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, oldSessionID, directScope, intent.ID, 100, 4, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, oldSessionID); err != nil {
		t.Fatal(err)
	}
	manifestScopeID, err := protectionScopeID(root.Authority, root.Repository, "sha256:"+strings.Repeat("e", 64), root.Generation)
	if err != nil {
		t.Fatal(err)
	}
	root.ScopeID = manifestScopeID
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	manifest := &stagedManifest{
		scopeID: manifestScopeID, sessionID: stagedSessionID(manifestScopeID), scopeBytes: 500,
		blobs: []contentRef{{digest: blobDigest, size: 100}},
	}
	ready, err := processor.admitManifestScope(ctx, &root, rootLease, manifest, manifest.scopeBytes, 4, 1<<20, false)
	if err != nil || !ready {
		t.Fatalf("admit manifest scope = ready %t, err %v", ready, err)
	}
	_, active, err := gate.protectionSession(ctx, oldSessionID)
	oldSession, readErr := gate.readProtectionSession(ctx, oldSessionID)
	if err != nil || readErr != nil || active || oldSession.State != "transferred" {
		t.Fatalf("old direct session = %#v active=%t err=%v readErr=%v; want transferred and inactive", oldSession, active, err, readErr)
	}
	currentIntent, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || currentIntent.ProtectionSessionID != manifest.sessionID {
		t.Fatalf("intent protection session = %q err=%v; want %q", currentIntent.ProtectionSessionID, err, manifest.sessionID)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, manifest.sessionID)
	if err != nil || !containsString(owners, intent.ID) || !containsString(owners, root.ID) {
		t.Fatalf("replacement session owners = %v err=%v; want blob intent %q and root %q", owners, err, intent.ID, root.ID)
	}
}

func TestAdmitBlobProtectionMakesBlobTaskSessionOwner(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, gate: gate, records: tasks.NewRecords(store)}
	processor := &solidifier{service: service, gate: gate}
	manager, err := tasks.NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	processor.tasks = manager
	service.tasks = manager

	root, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", "sha256:"+strings.Repeat("e", 64), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("d", 64)}
	intent, _, err := service.records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	blobTask, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	scopeID := "manifest-scope"
	sessionID := stagedSessionID(scopeID)
	if err := gate.AdmitProtection(ctx, sessionID, scopeID, root.ID, 128, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := service.records.LinkBlobDemandScope(ctx, intent.ID, stagedDemandID(root.ID, scopeID), root.ID, scopeID, sessionID, 128); err != nil {
		t.Fatal(err)
	}
	processor.cfg = config.Config{Solidification: config.Solidification{
		MaxProtectedCacheBytes: 1024, MaxProtectionSessions: 4, MaxUnanchoredDistributionBytes: 1024,
	}}
	protection, err := processor.admitBlobProtection(ctx, &blobTask, intent)
	if err != nil {
		t.Fatalf("admit blob protection: %v", err)
	}
	if protection.ownerID != blobTask.ID {
		t.Fatalf("blob protection owner = %q, want blob task %q", protection.ownerID, blobTask.ID)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(owners, intent.ID) || !containsString(owners, root.ID) {
		t.Fatalf("protection session owners = %v, want blob task %q and root %q", owners, intent.ID, root.ID)
	}
	if remaining, err := gate.RemoveProtectionOwner(ctx, sessionID, root.ID); err != nil || remaining != 1 {
		t.Fatalf("remove cancelled root owner: remaining=%d err=%v, want blob task to retain scope", remaining, err)
	}
	if _, active, err := gate.protectionSession(ctx, sessionID); err != nil || !active {
		t.Fatalf("scope after root cancellation: active=%t err=%v", active, err)
	}
}
