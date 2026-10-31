package app

import (
	"context"
	"errors"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestConfirmInactiveProtectionSessionReleasedAcceptsTransferredSession(t *testing.T) {
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
	oldID, newID, ownerID := mustGateTestID(t), mustGateTestID(t), mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, oldID, "blob:sha256:old", ownerID, 100, 2, 1000); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReplaceProtection(ctx, oldID, newID, "manifest:sha256:new", 200, 2, 1000); err != nil {
		t.Fatal(err)
	}

	if err := gate.confirmInactiveProtectionSessionReleased(ctx, oldID, "", ""); err != nil {
		t.Fatalf("confirm transferred source session = %v", err)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, newID)
	if err != nil || len(owners) != 1 || owners[0] != ownerID {
		t.Fatalf("replacement owners = %v, error = %v", owners, err)
	}
	if state := gate.State(); state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 200 {
		t.Fatalf("coordinator after source confirmation = %#v", state)
	}
}

func TestConfirmInactiveProtectionSessionReleasedRejectsUnmarkedTransferredSession(t *testing.T) {
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
	oldID, newID, ownerID := mustGateTestID(t), mustGateTestID(t), mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, oldID, "blob:sha256:old", ownerID, 100, 2, 1000); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReplaceProtection(ctx, oldID, newID, "manifest:sha256:new", 200, 2, 1000); err != nil {
		t.Fatal(err)
	}
	session, err := gate.readProtectionSession(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	session.TerminalReason = "invalid"
	if err := gate.saveProtectionSession(ctx, &session); err != nil {
		t.Fatal(err)
	}

	if err := gate.confirmInactiveProtectionSessionReleased(ctx, oldID, "", ""); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("confirm invalid transferred source session = %v, want terminal-reason error", err)
	}
}

func TestSolidifiedBlobTaskCleansTransferredProtectionReference(t *testing.T) {
	ctx := context.Background()
	fixture := newStagedAcceptanceFixture(t, 1024)
	data := []byte("solidified blob content")
	digest := artifact.CalculateSHA256(data)
	intent, err := fixture.service.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err = fixture.service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	oldID, newID := mustGateTestID(t), mustGateTestID(t)
	if err := fixture.service.gate.AdmitProtection(ctx, oldID, "blob:"+digest, intent.ID, int64(len(data)), 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.records.SetBlobIntentProtectionSession(ctx, intent.ID, oldID); err != nil {
		t.Fatal(err)
	}
	blobTask, created, err := fixture.service.enqueueBlobIntent(ctx, intent)
	if err != nil || !created {
		t.Fatalf("enqueue protected blob task = %#v, created=%t, error=%v", blobTask, created, err)
	}
	if err := fixture.service.gate.ReplaceProtection(ctx, oldID, newID, "manifest:sha256:root", int64(len(data))*2, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.tasks.AnchorBlobTask(ctx, blobTask.ID); err != nil {
		t.Fatal(err)
	}
	blobTask, err = fixture.service.tasks.Get(ctx, blobTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.processor.TaskTerminal(ctx, &blobTask); err != nil {
		t.Fatalf("terminal cleanup for blob task with transferred protection = %v", err)
	}
	completed, err := fixture.service.tasks.Get(ctx, blobTask.ID)
	if err != nil || !completed.TerminalCleanupComplete || completed.GCProtected || len(completed.SessionIDs) != 0 {
		t.Fatalf("completed blob task = %#v, error = %v", completed, err)
	}
	if state := fixture.service.gate.State(); state.RecoveryBarrier || state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("coordinator after terminal cleanup = %#v", state)
	}
}
