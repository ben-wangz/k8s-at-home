package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/tasks"
)

func TestVerifiedBlobYieldWakesCacheWaiterAfterRunningHeadReleases(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	headIntent := newReadyBlobIntent(t, ctx, manager, records)
	head, err := manager.Get(ctx, headIntent.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitingIntent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("8", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	waitingIntent, err = records.SetBlobIntentReady(ctx, waitingIntent.ID, "application/octet-stream", "cache-v1", 128)
	if err != nil {
		t.Fatal(err)
	}
	waiter, _, err := manager.EnqueueBlob(ctx, waitingIntent)
	if err != nil {
		t.Fatal(err)
	}
	headLease, err := manager.Leases().Acquire(ctx, "tasks/"+head.ID, "test-cache-head", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, headLease)
	waiterLease, err := manager.Leases().Acquire(ctx, "tasks/"+waiter.ID, "test-cache-waiter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, waiterLease)
	head.Status = tasks.Running
	head.QuotaWaitAt = time.Now().Add(-time.Minute).UTC()
	head.QuotaWaitReason = "protected_cache_capacity"
	if err := manager.SaveProgress(ctx, &head, headLease); err != nil {
		t.Fatal(err)
	}
	waiter.Status, waiter.WaitReason = tasks.WaitingQuota, "protected_cache_capacity"
	waiter.QuotaWaitAt, waiter.QuotaWaitReason = time.Now().UTC(), waiter.WaitReason
	if err := manager.SaveProgress(ctx, &waiter, waiterLease); err != nil {
		t.Fatal(err)
	}
	if err := service.gate.ReserveProtectedCache(ctx, head.ID, 128, 1024); err != nil {
		t.Fatal(err)
	}
	processor := &solidifier{service: service, tasks: manager, gate: service.gate}
	if err := processor.releaseProtectedCacheReservation(ctx, &head); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Get(ctx, waiter.ID)
	if err != nil || before.Status != tasks.WaitingQuota {
		t.Fatalf("waiter before persisted yield = %s, %v", before.Status, err)
	}
	head, err = manager.Get(ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	head.Status, head.WaitReason = tasks.VerifiedLocal, "awaiting_manifest_anchor"
	head.QuotaWaitAt, head.QuotaWaitReason = time.Time{}, ""
	if err := manager.SaveProgress(ctx, &head, headLease); err != nil {
		t.Fatal(err)
	}
	processor.TaskYielded(ctx, &head)
	after, err := manager.Get(ctx, waiter.ID)
	if err != nil || after.Status != tasks.Pending || after.WaitReason != "" {
		t.Fatalf("waiter after persisted yield = %#v, %v; want pending", after, err)
	}
	if service.gate.HasProtectedCacheReservation(head.ID) {
		t.Fatal("verified task retained its cache reservation")
	}
}
