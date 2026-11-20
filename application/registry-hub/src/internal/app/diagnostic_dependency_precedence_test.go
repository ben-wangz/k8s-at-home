package app

import (
	"context"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestBlobDependencyPrioritizesNotificationOverTaskWait(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("6", 64)}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, identity, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = f.service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := f.service.tasks.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "notification-precedence-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.WaitReason, task.WaitResourceKey = tasks.WaitingDeps, "object_lease_pending", "objects/ghcr.io/team/image"
	if err := f.service.tasks.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	_ = f.service.tasks.Leases().Release(ctx, taskLease)
	intent, err = f.service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentVerified, "", "")
	if err != nil {
		t.Fatal(err)
	}
	source := blobNotification{IntentID: intent.ID, Identity: identity, State: intent.State, Generation: intent.Generation, CacheVersion: intent.CacheVersion, DemandRevision: intent.DemandRevision}
	if _, err := (workqueue.Queue{Store: f.service.store}).Enqueue(ctx, "notify", notificationSource(intent, ""), source, intent.UpdatedAt); err != nil {
		t.Fatal(err)
	}

	view := dependencyView{RegistryState: "unknown", CacheState: "missing", ReadErrors: map[string]string{}}
	f.service.enrichIntentDependency(ctx, intent, &view)
	if view.TaskWaitReason != "object_lease_pending" || view.Reason != "notification_pending" || view.NextAction != "wait_dependency_notification" {
		t.Fatalf("notification and task wait evidence = %#v", view)
	}
	verified := dependencyView{RegistryState: "verified", Reason: "available", NextAction: "none", CacheState: "missing", ReadErrors: map[string]string{}}
	f.service.enrichIntentDependency(ctx, intent, &verified)
	if verified.Reason != "available" || verified.NextAction != "none" || verified.NotificationWork == nil {
		t.Fatalf("registry verification lost precedence or notification evidence: %#v", verified)
	}
}
