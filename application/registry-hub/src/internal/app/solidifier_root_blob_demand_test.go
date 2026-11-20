package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestRootDemandCreatesFreshRoundForClosedHistory(t *testing.T) {
	for _, mode := range []string{"failed_intent", "terminal_task"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			service, manager, processor, root, rootLease := newRunningRootDemandFixture(t)
			defer manager.Leases().Release(ctx, rootLease)
			body := []byte("data")
			identity := tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: artifact.CalculateSHA256(body)}
			old, _, err := service.records.EnsureBlobIntent(ctx, identity, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "failed_intent" {
				if _, err := service.records.SetBlobIntentState(ctx, old.ID, tasks.IntentFailed, "upstream", "failed"); err != nil {
					t.Fatal(err)
				}
			} else {
				old, err = service.records.SetBlobIntentReady(ctx, old.ID, "application/octet-stream", "cache-v1", 4)
				if err != nil {
					t.Fatal(err)
				}
				blobTask, _, err := manager.EnqueueBlob(ctx, old)
				if err != nil {
					t.Fatal(err)
				}
				blobLease, err := manager.Leases().Acquire(ctx, "tasks/"+blobTask.ID, "terminal-blob-test", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				blobTask.Status = tasks.Failed
				if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
					t.Fatal(err)
				}
				_ = manager.Leases().Release(ctx, blobLease)
			}

			scope := &stagedManifest{ref: contentRef{digest: "sha256:" + strings.Repeat("b", 64), size: 128}, blobs: []contentRef{{digest: identity.Digest, size: 4}}}
			if mode == "failed_intent" {
				if err := processor.assignStagedBlobScopes(ctx, &root, rootLease, &stagedGraph{manifests: []*stagedManifest{scope}}); err != nil {
					t.Fatalf("assign root blob scopes: %v", err)
				}
			} else {
				if err := service.cache.Put(ctx, root.Authority, root.Repository, identity.Digest, "application/octet-stream", strings.NewReader(string(body)), int64(len(body))); err != nil {
					t.Fatal(err)
				}
				scope.scopeID = "scope-test"
				scope.sessionID = strings.Repeat("c", 32)
				scope.scopeBytes = 132
				ready, current, err := processor.prepareStagedBlob(ctx, &root, rootLease, scope, contentRef{digest: identity.Digest, size: 4})
				if err != nil || !ready || current.ID == old.ID || current.Round != old.Round+1 {
					t.Fatalf("prepare cached blob = %#v ready=%t err=%v; want fresh round", current, ready, err)
				}
			}
			current, err := service.records.GetBlobIntentByIdentity(ctx, identity)
			if err != nil || current.ID == old.ID || current.Round != old.Round+1 {
				t.Fatalf("current blob round = %#v err=%v; want a new round after %q", current, err, old.ID)
			}
			demands, _, err := service.records.ListBlobDemandRecords(ctx, current.ID, "", 10)
			if err != nil || len(demands) != 1 || demands[0].RootTaskID != root.ID {
				t.Fatalf("new round demands = %#v err=%v; want this root linked", demands, err)
			}
		})
	}
}

func TestRootDemandWaitsForCleanupOrRepairAdmission(t *testing.T) {
	for _, mode := range []string{"cancel_requested", "repair_pending"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			service, manager, processor, root, rootLease := newRunningRootDemandFixture(t)
			defer manager.Leases().Release(ctx, rootLease)
			identity := tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: "sha256:" + strings.Repeat("d", 64)}
			intent, _, err := service.records.EnsureBlobIntent(ctx, identity, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 4)
			if err != nil {
				t.Fatal(err)
			}
			blobTask, _, err := manager.EnqueueBlob(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			blobLease, err := manager.Leases().Acquire(ctx, "tasks/"+blobTask.ID, "admission-wait-test", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel_requested" {
				blobTask.Status, blobTask.CancelRequested = tasks.Running, true
			} else {
				blobTask.Status = tasks.RepairPending
			}
			if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
				t.Fatal(err)
			}
			_ = manager.Leases().Release(ctx, blobLease)

			_, err = processor.resolveRootBlobDemand(ctx, &root, rootLease, contentRef{digest: identity.Digest, size: 4}, "scope", strings.Repeat("e", 32), 4)
			var yielded *tasks.Yield
			if !errors.As(err, &yielded) || yielded.Reason != "blob_intent_admission" || yielded.ResourceKey != "blob-intent/"+intent.ID {
				t.Fatalf("root demand result = %v, want resource wait for %s", err, intent.ID)
			}
			currentRoot, err := manager.Get(ctx, root.ID)
			if err != nil || currentRoot.Status != tasks.Running || currentRoot.RetryCount != root.RetryCount {
				t.Fatalf("admission wait changed root retry state: task=%+v err=%v", currentRoot, err)
			}
			current, err := service.records.GetBlobIntentByIdentity(ctx, identity)
			if err != nil || current.ID != intent.ID || current.Round != intent.Round {
				t.Fatalf("cleanup wait changed current round = %#v err=%v", current, err)
			}
			demands, _, err := service.records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
			if err != nil || len(demands) != 0 {
				t.Fatalf("cleanup wait linked demand = %#v err=%v; want no partial association", demands, err)
			}
		})
	}
}

func TestRootDemandAdmissionWaitWakesAfterBlobCleanup(t *testing.T) {
	ctx := context.Background()
	service, manager, _, root, rootLease := newRunningRootDemandFixture(t)
	defer manager.Leases().Release(ctx, rootLease)
	identity := tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: "sha256:" + strings.Repeat("a", 64)}
	intent, _, err := service.records.EnsureBlobIntent(ctx, identity, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 4)
	if err != nil {
		t.Fatal(err)
	}
	blobTask, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	blobLease, err := manager.Leases().Acquire(ctx, "tasks/"+blobTask.ID, "admission-cleanup-wake-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("b", 32)
	blobTask.Status, blobTask.CancelRequested, blobTask.CancelPhase = tasks.Cancelled, true, "quiesced"
	blobTask.TerminalCleanupID = "cleanup-pending"
	blobTask.GCProtected, blobTask.ProtectionSessionID, blobTask.ProtectionOwnerID = true, sessionID, intent.ID
	blobTask.ScopeID, blobTask.SessionIDs, blobTask.RequestedBytes, blobTask.ReservedBytes = "scope-old", []string{sessionID}, 4, 4
	if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
		t.Fatal(err)
	}
	_ = manager.Leases().Release(ctx, blobLease)
	if _, err := service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentCancelled, "cancelled", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := service.records.SetBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}

	root.Status, root.WaitReason, root.WaitResourceKey = tasks.WaitingDeps, "blob_intent_admission", "blob-intent/"+intent.ID
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileTaskResourceWait(ctx, root); err != nil {
		t.Fatal(err)
	}
	waiting, err := manager.Get(ctx, root.ID)
	if err != nil || waiting.Status != tasks.WaitingDeps {
		t.Fatalf("root woke before protection cleanup: task=%+v err=%v", waiting, err)
	}

	currentBlob, err := manager.Get(ctx, blobTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveTerminalProtection(ctx, &currentBlob, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := service.records.ClearBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileTaskResourceWait(ctx, root); err != nil {
		t.Fatal(err)
	}
	woken, err := manager.Get(ctx, root.ID)
	if err != nil || woken.Status != tasks.Pending || woken.WaitReason != "" || woken.WaitResourceKey != "" {
		t.Fatalf("root did not wake after protection cleanup: task=%+v err=%v", woken, err)
	}
}

func newRunningRootDemandFixture(t *testing.T) (*Service, *tasks.Manager, *solidifier, tasks.Task, lease.Lease) {
	t.Helper()
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	service := &Service{store: store, records: tasks.NewRecords(store), tasks: manager, cache: artifact.NewCache(store)}
	root, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "root-demand", "sha256:"+strings.Repeat("f", 64), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	root.Status, root.AttemptID = tasks.Running, "root-attempt"
	rootLease, err := manager.Leases().Acquire(ctx, "tasks/"+root.ID, "root-demand-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	processor := &solidifier{service: service, tasks: manager, cache: service.cache}
	return service, manager, processor, root, rootLease
}
