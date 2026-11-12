package app

import (
	"context"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestReadyBlobIntentWakesExistingWaitingCacheTask(t *testing.T) {
	for _, protected := range []bool{false, true} {
		name := "direct"
		if protected {
			name = "protected"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			service, manager, records := newTerminalDemandFixture(t)
			intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
				Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64),
			}, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
			if err != nil {
				t.Fatal(err)
			}

			if protected {
				sessionID, scopeID := strings.Repeat("b", 32), "test-scope"
				if err := service.gate.AdmitProtection(ctx, sessionID, scopeID, intent.ID, 64, 2, 1024); err != nil {
					t.Fatal(err)
				}
				if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
					t.Fatal(err)
				}
				intent, err = records.GetBlobIntent(ctx, intent.ID)
				if err != nil {
					t.Fatal(err)
				}
			}

			if _, _, err := service.enqueueBlobIntent(ctx, intent); err != nil {
				t.Fatal(err)
			}
			if err := saveDemandRootStatus(ctx, manager, intent.ID, tasks.WaitingCache, "cache_entry_unavailable"); err != nil {
				t.Fatal(err)
			}
			if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentWaitingCache, "", ""); err != nil {
				t.Fatal(err)
			}
			ready, err := records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
			if err != nil {
				t.Fatal(err)
			}

			if _, _, err := service.enqueueBlobIntent(ctx, ready); err != nil {
				t.Fatal(err)
			}
			current, err := manager.Get(ctx, intent.ID)
			if err != nil || current.Status != tasks.Pending || current.WaitReason != "" {
				t.Fatalf("task after cache readiness = %#v, %v; want pending", current, err)
			}
		})
	}
}

func TestReadyBlobIntentWaitingCacheWakePreservesConcurrentTaskState(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("c", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.enqueueBlobIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	task, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-running-cache-wake", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.WaitReason = tasks.Running, ""
	if err := manager.SaveProgress(ctx, &task, lease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentWaitingCache, "", ""); err != nil {
		t.Fatal(err)
	}
	ready, err := records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.enqueueBlobIntent(ctx, ready); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Get(ctx, intent.ID)
	if err != nil || current.Status != tasks.Running {
		t.Fatalf("running task after cache readiness = %#v, %v; want unchanged", current, err)
	}
}
