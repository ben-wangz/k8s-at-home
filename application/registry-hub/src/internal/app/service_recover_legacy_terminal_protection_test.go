package app

import (
	"context"
	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestStartupRecoveryReconcilesLegacyAggregateProtectionBeforeTerminalRelease(t *testing.T) {
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

	rootDigest := "sha256:" + strings.Repeat("a", 64)
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", rootDigest, "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	task.ProtocolVersion = 1
	task.Status = tasks.Failed
	task.Phase = "solidification"
	task.Graph[rootDigest] = tasks.GraphEntry{Digest: rootDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 100}
	task.Checkpoints[rootDigest] = "uploaded"
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetLegacyProtectionBytes(ctx, task.ID, 100); err != nil {
		t.Fatal(err)
	}

	service := &Service{
		store: store, gate: gate, tasks: manager, records: tasks.NewRecords(store),
		cfg: config.Config{Limits: config.Limits{ClosureBytes: 1 << 20}},
	}
	if err := service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatalf("recoverSolidificationProtection() error = %v", err)
	}
	if gate.State().RecoveryBarrier {
		t.Fatal("startup recovery barrier remained active after terminal legacy task was reconciled")
	}
	current, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.GCProtected || current.RecoveryReason != "" || len(current.SessionIDs) != 0 || current.LegacyProtectionBytes != 100 {
		t.Fatalf("legacy task after recovery = %#v", current)
	}
}
