package app

import (
	"context"
	"errors"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func TestDistributionVerificationWakePreservesOtherWaits(t *testing.T) {
	for _, state := range []struct {
		status tasks.State
		reason string
	}{{tasks.WaitingQuota, "distribution_quota"}, {tasks.WaitingCache, "cache_entry_unavailable"}, {tasks.WaitingDeps, "dependencies_unavailable"}} {
		t.Run(string(state.status)+"_"+state.reason, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
			root := f.root("waiting", manifest)
			ctx := context.Background()
			if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
				task.Status, task.WaitReason = state.status, state.reason
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.service.wakeVerificationTask(ctx, root); err != nil {
				t.Fatal(err)
			}
			current, err := f.service.tasks.Get(ctx, root.ID)
			if err != nil || current.Status != state.status || current.WaitReason != state.reason {
				t.Fatalf("unrelated wait changed: %+v, %v", current, err)
			}
		})
	}
}

func TestVerificationReconciliationWaitsForLiveWorkAndRecoversMissingWork(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
	root := f.root("verification-wait", manifest)
	ctx := context.Background()
	body := []byte("verification waiting")
	digest := artifact.CalculateSHA256(body)
	blob := contentRef{digest: digest, size: int64(len(body))}
	if err := f.processor.verifyOrReuseLocalBlob(ctx, &root, blob, "", "/unused"); err == nil || root.VerificationWorkID == "" {
		t.Fatalf("work was not recorded: %v", err)
	}
	if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
		task.Status, task.WaitReason, task.VerificationWorkID = tasks.WaitingDeps, "distribution_verification", root.VerificationWorkID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.tasks.Get(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileVerificationWait(ctx, current); err != nil {
		t.Fatal(err)
	}
	current, err = f.service.tasks.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.WaitingDeps {
		t.Fatalf("live verification caused a premature wake: %+v, %v", current, err)
	}
	if err := f.service.store.Delete(ctx, workqueue.Key("distribution_verification", root.VerificationWorkID)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileVerificationWait(ctx, current); err != nil {
		t.Fatal(err)
	}
	current, err = f.service.tasks.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.Pending {
		t.Fatalf("missing verification did not trigger recovery: %+v, %v", current, err)
	}
}

func TestDeferredVerificationReportsInvalidObjectToSourceTask(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
	root := f.root("invalid-deferred", manifest)
	ctx := context.Background()
	body := []byte("expected object")
	digest := artifact.CalculateSHA256(body)
	f.objects[digest] = stagedAcceptanceObject{body: []byte("damaged object!"), mediaType: "application/octet-stream"}
	blob := contentRef{digest: digest, size: int64(len(body))}
	if err := f.processor.verifyOrReuseLocalBlob(ctx, &root, blob, "", "/unused"); err == nil {
		t.Fatal("missing verification was not deferred")
	}
	if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
		task.Status, task.WaitReason, task.VerificationWorkID = tasks.WaitingDeps, "distribution_verification", root.VerificationWorkID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queue := f.service.backgroundQueue()
	job := readDistributionVerificationJob(t, f)
	job.LeaseOwner = "test"
	claimed, err := queue.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	progress, done, err := f.service.distributionVerificationStep(ctx, &claimed)
	if err != nil || !done {
		t.Fatalf("invalid verification did not produce a result: %t, %v", done, err)
	}
	if err := queue.Finish(ctx, &claimed, progress, done, nil); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.tasks.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.Pending || current.VerificationWorkID == "" {
		t.Fatalf("verification result did not wake its task: %+v, %v", current, err)
	}
	err = f.processor.consumeVerificationResult(ctx, &current)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "distribution_invalid" {
		t.Fatalf("invalid Distribution object was not reported as a permanent failure: %v", err)
	}
	stale := root
	stale.Generation++
	if err := f.processor.consumeVerificationResult(ctx, &stale); err != nil {
		t.Fatalf("old failure applied to a new generation: %v", err)
	}
}
