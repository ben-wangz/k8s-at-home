package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestBlobWorkerProgressSurvivesIdenticalProtectionReconciliation(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	blob := f.cacheObject([]byte("blob owner identity"), "application/vnd.oci.image.config.v1+json")
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root(descriptor.Digest, descriptor)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("prepare manifest scope: %v", err)
	}
	f.transfer(blob)
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{
		Authority: root.Authority, Repository: root.Repository, Digest: blob.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.service.tasks.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ProtectionOwnerID != task.ID {
		t.Errorf("persisted blob owner = %q, want blob task %q", task.ProtectionOwnerID, task.ID)
	}
	owners, err := f.service.gate.ProtectionSessionOwners(ctx, task.ProtectionSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(owners, task.ID) {
		t.Fatalf("protection owners = %v, want blob task %q", owners, task.ID)
	}
	taskLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "owner-regression", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, taskLease)
	key := "tasks/state/" + task.ID + ".json"
	before, err := f.service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.service.tasks.ReconcileProtectionState(ctx, task.ID, task.ScopeID, task.SessionIDs, task.RequestedBytes, task.ReservedBytes); err != nil {
			t.Fatal(err)
		}
		after, err := f.service.store.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		latest, err := f.service.tasks.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != before.Version || !latest.UpdatedAt.Equal(task.UpdatedAt) {
			t.Errorf("identical protection reconciliation rewrote task: version %q -> %q, updated %s -> %s", before.Version, after.Version, task.UpdatedAt, latest.UpdatedAt)
		}
	}
	if err := f.service.tasks.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatalf("identical protection reconciliation invalidated worker progress: %v", err)
	}
}
