package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestLocalBlobObservationSkipsRepeatedDistributionBodyRead(t *testing.T) {
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
	records := tasks.NewRecords(store)
	service := &Service{store: store, records: records}
	var bodyReads atomic.Int32
	body := []byte("verified local layer")
	digest := artifact.CalculateSHA256(body)
	registry := newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("Distribution method = %s, want GET", request.Method)
		}
		bodyReads.Add(1)
		writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
		writer.Header().Set("Docker-Content-Digest", digest)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	}))
	processor := &solidifier{service: service, registry: registry, gate: gate}
	task := &tasks.Task{ID: strings.Repeat("a", 32), Authority: "ghcr.io", Repository: "team/image"}
	blob := contentRef{digest: digest, size: int64(len(body))}
	path := "/v2/local/blobs/" + digest

	for range 2 {
		if err := processor.verifyOrReuseLocalBlob(ctx, task, blob, task.ID, path); err != nil {
			t.Fatalf("verify local blob: %v", err)
		}
	}
	if got := bodyReads.Load(); got != 1 {
		t.Fatalf("full Distribution body reads = %d, want one read for two observations", got)
	}
}

func TestPrepareStagedBlobLinksRootScopeDemand(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := artifact.NewCache(store)
	authority, repository := "ghcr.io", "team/image"
	body := []byte("cached layer")
	digest := artifact.CalculateSHA256(body)
	if err := cache.Put(ctx, authority, repository, digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	records := tasks.NewRecords(store)
	service := &Service{store: store, records: records}
	processor := &solidifier{service: service, cache: cache}
	task := &tasks.Task{ID: strings.Repeat("a", 32), Authority: authority, Repository: repository}
	scopeID, err := protectionScopeID(authority, repository, "sha256:"+"aabbccdd", 0)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &stagedManifest{
		scopeID: scopeID, sessionID: stagedSessionID(scopeID), scopeBytes: int64(len(body) + 256),
	}
	ready, intent, err := processor.prepareStagedBlob(ctx, task, manifest, contentRef{digest: digest, size: int64(len(body))})
	if err != nil {
		t.Fatalf("prepare cached blob: %v", err)
	}
	if !ready || intent.State != tasks.IntentReady {
		t.Fatalf("prepared intent = %#v, ready = %v", intent, ready)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(demands) != 1 {
		t.Fatalf("blob demand count = %d, want 1", len(demands))
	}
	demand := demands[0]
	if demand.RootTaskID != task.ID || demand.ScopeID != manifest.scopeID || demand.SessionID != manifest.sessionID || demand.ReservedBytes != manifest.scopeBytes {
		t.Fatalf("blob demand = %#v, want root scope admission details", demand)
	}
}

func TestManifestQuotaWaitPersistsExactReplacementCredit(t *testing.T) {
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
	root, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	blobDigest := "sha256:" + strings.Repeat("d", 64)
	identity := tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: blobDigest}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, root.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	directScope, err := protectionScopeID(root.Authority, root.Repository, blobDigest, root.Generation)
	if err != nil {
		t.Fatal(err)
	}
	oldSessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, oldSessionID, directScope, intent.ID, 100, 2, 1000); err != nil {
		t.Fatal(err)
	}
	if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, oldSessionID); err != nil {
		t.Fatal(err)
	}
	otherSessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, otherSessionID, "other-scope", mustGateTestID(t), 100, 2, 1000); err != nil {
		t.Fatal(err)
	}
	manifestScopeID, err := protectionScopeID(root.Authority, root.Repository, "sha256:"+strings.Repeat("e", 64), root.Generation)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &stagedManifest{scopeID: manifestScopeID, sessionID: stagedSessionID(manifestScopeID), scopeBytes: 950, blobs: []contentRef{{digest: blobDigest, size: 100}}}
	_, err = processor.admitManifestScope(ctx, &root, lease.Lease{}, manifest, 950, 1, 1000, false)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.WaitingQuota {
		t.Fatalf("admitManifestScope() error = %v, want a distribution quota wait", err)
	}
	if root.QuotaWaitBytes != 950 || root.QuotaReplaceableBytes != 100 || root.QuotaReplaceableSlots != 1 {
		t.Fatalf("persisted quota wait hint = bytes %d, replaceable bytes %d, slots %d", root.QuotaWaitBytes, root.QuotaReplaceableBytes, root.QuotaReplaceableSlots)
	}
	if root.QuotaWaitAt.IsZero() || root.QuotaWaitReason != "distribution_quota" {
		t.Fatalf("quota queue position = %s/%q, want a durable distribution queue entry", root.QuotaWaitAt, root.QuotaWaitReason)
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
