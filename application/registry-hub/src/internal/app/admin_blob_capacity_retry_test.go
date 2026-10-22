package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestRetryBlobCapacityFailureStartsFreshRound(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	records := tasks.NewRecords(store)
	cache := artifact.NewCache(store)
	body := []byte("retained blob input")
	identity := tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: artifact.CalculateSHA256(body),
	}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, identity.Authority, identity.Repository, identity.Digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.GetAnyRetained(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	_ = cached.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	oldTask, _, err := manager.EnqueueBlob(ctx, ready)
	if err != nil {
		t.Fatal(err)
	}
	oldTask.Status = tasks.Failed
	oldTask.ErrorClass = "capacity_limit"
	workerLease, err := manager.Leases().Acquire(ctx, "tasks/"+oldTask.ID, "admin-retry-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &oldTask, workerLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, workerLease); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, oldTask.ID, tasks.IntentFailed, "capacity_limit", "limit exceeded"); err != nil {
		t.Fatal(err)
	}

	service := &Service{tasks: manager, records: records, cache: cache}
	newTask, err := service.retryTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("retry blob task: %v", err)
	}
	if newTask.ID == oldTask.ID || newTask.Kind != tasks.BlobKind || newTask.Status != tasks.Pending {
		t.Fatalf("fresh blob task = %#v", newTask)
	}
	newIntent, err := records.GetBlobIntent(ctx, newTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newIntent.Round != 2 || newIntent.State != tasks.IntentReady {
		t.Fatalf("fresh blob intent = %#v", newIntent)
	}
	old, err := manager.Get(ctx, oldTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != tasks.Failed {
		t.Fatalf("old blob task status = %q, want failed", old.Status)
	}
}
