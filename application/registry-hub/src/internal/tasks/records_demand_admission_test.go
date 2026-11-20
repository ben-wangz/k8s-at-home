package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestNewBlobRoundWaitsForCancelledTaskProtectionCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("c", 64)}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 7, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.EnqueueBlob(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, IntentCancelled, "cancelled", "cancelled"); err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("d", 32)
	if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	task, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "demand-cleanup-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.CancelRequested, task.CancelPhase = Cancelled, true, "quiesced"
	task.GCProtected, task.ProtectionSessionID, task.ProtectionOwnerID = true, sessionID, intent.ID
	task.ScopeID, task.SessionIDs, task.RequestedBytes, task.ReservedBytes = "old-scope", []string{sessionID}, 64, 64
	task.TerminalCleanupID = "cleanup-pending"
	if err := manager.SaveProgress(ctx, &task, owner); err != nil {
		t.Fatal(err)
	}
	_ = manager.Leases().Release(ctx, owner)

	newDemand := strings.Repeat("e", 32)
	if _, _, err := records.EnsureBlobIntent(ctx, identity, 7, newDemand); !errors.Is(err, ErrDemandAdmissionWaiting) {
		t.Fatalf("new round admission error = %v, want cleanup wait", err)
	}
	current, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveTerminalProtection(ctx, &current, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := records.ClearBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	next, created, err := records.EnsureBlobIntent(ctx, identity, 7, newDemand)
	if err != nil || !created || next.Round != intent.Round+1 || next.ID == intent.ID {
		t.Fatalf("new round after cleanup = %#v created=%t err=%v", next, created, err)
	}
}
