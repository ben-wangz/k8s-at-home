package tasks

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestReconcileProtectionStateDoesNotRewriteUnchangedTask(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	ctx := context.Background()
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileProtectionState(ctx, task.ID, "scope", []string{sessionID}, 64, 64); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileProtectionState(ctx, task.ID, "scope", []string{sessionID}, 64, 64); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.version != before.version || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("idempotent protection reconciliation rewrote task: before version=%q updated=%s, after version=%q updated=%s", before.version, before.UpdatedAt, after.version, after.UpdatedAt)
	}
}
