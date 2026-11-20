package tasks

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestSaveBlobProgressMergesMatchingProtectionReconciliation(t *testing.T) {
	for _, stage := range []string{"version_check", "conditional_write"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			baseStore, manager, intent, task, taskLease, store := newBlobProgressMergeFixture(t)
			defer baseStore.Close()
			defer manager.Close()
			defer manager.Leases().Release(ctx, taskLease)
			base := task
			target := blobProgressTarget(base, "session-a")
			scopeID, sessionID := "scope-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if stage == "version_check" {
				if err := manager.ReconcileProtectionState(ctx, intent.ID, scopeID, []string{sessionID}, 64, 64); err != nil {
					t.Fatal(err)
				}
			} else {
				store.inject = true
				store.manager, store.taskID = manager, intent.ID
				store.scopeID, store.sessionID = scopeID, sessionID
			}
			if err := manager.SaveBlobProgressWithProtectionMerge(ctx, base, &target, taskLease); err != nil {
				t.Fatalf("save compatible blob progress: %v", err)
			}
			persisted, err := manager.Get(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if store.reconcileErr != nil {
				t.Fatalf("interleaved protection reconciliation: %v", store.reconcileErr)
			}
			if persisted.Checkpoints["blob"] != "verified" || persisted.AttemptID != base.AttemptID || persisted.RetryCount != base.RetryCount {
				t.Fatalf("progress commit = %#v; checkpoint, attempt and retry count were not preserved", persisted)
			}
			if persisted.ProtectionSessionID != sessionID || persisted.ProtectionOwnerID != intent.ID || persisted.ScopeID != scopeID ||
				len(persisted.SessionIDs) != 1 || persisted.SessionIDs[0] != sessionID || persisted.ReservedBytes != 64 || persisted.ProtectionAssociatedAt.IsZero() {
				t.Fatalf("reconciled protection association was not retained: %#v", persisted)
			}
		})
	}
}

func TestSaveBlobProgressRejectsIncompatibleOwnership(t *testing.T) {
	for _, mode := range []string{"different_session", "replacement_attempt", "cancelled", "lost_lease"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, manager, intent, task, taskLease, _ := newBlobProgressMergeFixture(t)
			defer store.Close()
			defer manager.Close()
			base := task
			target := blobProgressTarget(base, "session-a")
			switch mode {
			case "different_session":
				if err := manager.ReconcileProtectionState(ctx, intent.ID, "scope-b", []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, 64, 64); err != nil {
					t.Fatal(err)
				}
				if err := manager.SaveBlobProgressWithProtectionMerge(ctx, base, &target, taskLease); !errors.Is(err, ErrProtectionAdmissionConflict) {
					t.Fatalf("save with replacement session error=%v, want protection admission conflict", err)
				}
			case "replacement_attempt", "cancelled":
				current, err := manager.Get(ctx, intent.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "replacement_attempt" {
					current.AttemptID = "replacement-attempt"
				} else {
					current.CancelRequested = true
				}
				if err := manager.SaveProgress(ctx, &current, taskLease); err != nil {
					t.Fatal(err)
				}
				if err := manager.SaveBlobProgressWithProtectionMerge(ctx, base, &target, taskLease); !errors.Is(err, storage.ErrConditionFailed) {
					t.Fatalf("save after %s error=%v, want condition failure", mode, err)
				}
			case "lost_lease":
				if err := manager.Leases().Release(ctx, taskLease); err != nil {
					t.Fatal(err)
				}
				if err := manager.SaveBlobProgressWithProtectionMerge(ctx, base, &target, taskLease); !errors.Is(err, lease.ErrLost) {
					t.Fatalf("save after lease loss error=%v, want lease lost", err)
				}
			}
		})
	}
}

func newBlobProgressMergeFixture(t *testing.T) (*filesystem.Store, *Manager, BlobIntent, Task, lease.Lease, *protectionMergeFaultStore) {
	t.Helper()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &protectionMergeFaultStore{taskCreateFaultStore: taskCreateFaultStore{Store: base}}
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	records := NewRecords(store)
	intent, _, err := records.EnsureBlobIntent(ctx, BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64)}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "protection-merge-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.AttemptID = Running, "attempt-original"
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	return base, manager, intent, task, taskLease, store
}

func blobProgressTarget(base Task, session string) Task {
	target := base
	target.Checkpoints = map[string]string{"blob": "verified"}
	target.ScopeID = "scope-a"
	target.SessionIDs = []string{sessionIDForTest(session)}
	target.RequestedBytes, target.ReservedBytes = 64, 64
	target.GCProtected = true
	target.ProtectionSessionID, target.ProtectionOwnerID = sessionIDForTest(session), base.ID
	return target
}

func sessionIDForTest(value string) string {
	if value == "session-a" {
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}

type protectionMergeFaultStore struct {
	taskCreateFaultStore
	manager      *Manager
	taskID       string
	scopeID      string
	sessionID    string
	inject       bool
	reconcileErr error
}

func (s *protectionMergeFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.inject && key == taskKey(s.taskID) {
		s.inject = false
		s.reconcileErr = s.manager.ReconcileProtectionState(ctx, s.taskID, s.scopeID, []string{s.sessionID}, 64, 64)
		return storage.Entry{}, storage.ErrConditionFailed
	}
	return s.Store.Put(ctx, key, body, size, options)
}
