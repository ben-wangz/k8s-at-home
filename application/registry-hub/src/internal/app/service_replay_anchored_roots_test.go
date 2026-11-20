package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestReplayAnchoredRootsForRecoveredBlob(t *testing.T) {
	t.Run("requeues a current anchored root", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		_, blob, _ := publishAnchoredTestPlatform(f, "recovery-current")
		root := completedAnchoredRootForBlob(t, f, blob.Digest)

		if err := f.service.replayAnchoredRootsForRecoveredBlob(context.Background(), root.Authority, root.Repository, blob.Digest); err != nil {
			t.Fatalf("replay anchored root: %v", err)
		}
		active := activeRootForReference(t, f, root.Reference)
		if len(active) != 1 || active[0].ID == root.ID || active[0].RootDigest != root.RootDigest || active[0].Generation != root.Generation {
			t.Fatalf("replayed roots = %#v; want one new round for the current root", active)
		}
	})

	t.Run("skips a stale reference generation", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		_, blob, _ := publishAnchoredTestPlatform(f, "recovery-stale-generation")
		root := completedAnchoredRootForBlob(t, f, blob.Digest)
		if _, err := f.service.advanceGeneration(context.Background(), root.Authority, root.Repository, root.Reference, false); err != nil {
			t.Fatal(err)
		}

		if err := f.service.replayAnchoredRootsForRecoveredBlob(context.Background(), root.Authority, root.Repository, blob.Digest); err != nil {
			t.Fatalf("replay stale reference: %v", err)
		}
		if active := activeRootForReference(t, f, root.Reference); len(active) != 0 {
			t.Fatalf("stale reference created active roots: %#v", active)
		}
	})

	t.Run("skips stale graph evidence", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		_, blob, _ := publishAnchoredTestPlatform(f, "recovery-stale-graph")
		root := completedAnchoredRootForBlob(t, f, blob.Digest)
		node, err := f.service.records.GetGraphNode(context.Background(), root.ID, tasks.GraphBlob, blob.Digest)
		if err != nil {
			t.Fatal(err)
		}
		node.LocalState = "verified_present"
		if err := f.service.records.SaveGraphNode(context.Background(), &node); err != nil {
			t.Fatal(err)
		}

		if err := f.service.replayAnchoredRootsForRecoveredBlob(context.Background(), root.Authority, root.Repository, blob.Digest); err != nil {
			t.Fatalf("replay stale graph: %v", err)
		}
		if active := activeRootForReference(t, f, root.Reference); len(active) != 0 {
			t.Fatalf("stale graph created active roots: %#v", active)
		}
	})

	t.Run("does nothing without a prior root", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		digest := "sha256:" + strings.Repeat("d", 64)
		if err := f.service.replayAnchoredRootsForRecoveredBlob(context.Background(), "ghcr.io", "team/image", digest); err != nil {
			t.Fatalf("replay without prior root: %v", err)
		}
		active, err := f.service.tasks.Active(context.Background())
		if err != nil || len(active) != 0 {
			t.Fatalf("active tasks = %#v, %v; want none", active, err)
		}
	})

	t.Run("does not replace a conflicting active root", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		_, blob, _ := publishAnchoredTestPlatform(f, "recovery-active-conflict")
		root := completedAnchoredRootForBlob(t, f, blob.Digest)
		conflictingDigest := "sha256:" + strings.Repeat("e", 64)
		conflicting, _, err := f.service.tasks.EnqueueResolved(context.Background(), root.Authority, root.Repository,
			root.Reference, conflictingDigest, root.MediaType, root.Generation)
		if err != nil {
			t.Fatal(err)
		}

		if err := f.service.replayAnchoredRootsForRecoveredBlob(context.Background(), root.Authority, root.Repository, blob.Digest); err != nil {
			t.Fatalf("replay with active conflict: %v", err)
		}
		active := activeRootForReference(t, f, root.Reference)
		if len(active) != 1 || active[0].ID != conflicting.ID || active[0].RootDigest != conflictingDigest {
			t.Fatalf("active roots after conflict = %#v; want the existing conflicting task only", active)
		}
	})
}

func completedAnchoredRootForBlob(t *testing.T, f *stagedAcceptanceFixture, digest string) tasks.Task {
	t.Helper()
	references, _, err := f.service.records.ListCacheProtectionReferences(context.Background(), digest, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range references {
		root, err := f.service.tasks.Get(context.Background(), reference.TaskID)
		if err != nil || root.Kind != tasks.RootKind {
			continue
		}
		if root.Status != tasks.Solidified || !root.RootPublished {
			t.Fatalf("fixture root = %#v; want a published solidified root", root)
		}
		if !root.TerminalCleanupComplete {
			if err := f.service.tasks.SaveTerminalProtection(context.Background(), &root, false, ""); err != nil {
				t.Fatalf("complete fixture root cleanup: %v", err)
			}
		}
		return root
	}
	t.Fatal("no root task indexes the recovered blob")
	return tasks.Task{}
}

func activeRootForReference(t *testing.T, f *stagedAcceptanceFixture, reference string) []tasks.Task {
	t.Helper()
	all, _, err := f.service.tasks.List(context.Background(), "", "", 100)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	active := make([]tasks.Task, 0, 1)
	for _, task := range all {
		if task.Kind == tasks.RootKind && task.Reference == reference && (task.Status == tasks.Pending || task.Status == tasks.Running || task.Status == tasks.RetryWait || task.Status == tasks.WaitingDeps) {
			active = append(active, task)
		}
	}
	return active
}
