package tasks

import (
	"bytes"
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestDispatchRepairsCorruptAndLegacyCandidates(t *testing.T) {
	for _, mode := range []string{"corrupt", "legacy"} {
		t.Run(mode, func(t *testing.T) {
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
			task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", mode, "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
			if err != nil {
				t.Fatal(err)
			}
			key := dispatchCandidateKey(*task.Dispatch.Candidate)
			if mode == "corrupt" {
				entry, err := store.Head(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Put(ctx, key, bytes.NewBufferString("{"), 1, storage.PutOptions{IfMatch: entry.Version}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.Delete(ctx, key); err != nil {
					t.Fatal(err)
				}
				if err := inventory.Update(ctx, store, taskKey(task.ID), func(current *Task) error {
					current.Dispatch = nil
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			task, err = manager.Get(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.repairDispatchTask(ctx, &task); err != nil {
				t.Fatalf("repair %s candidate: %v", mode, err)
			}
			if task.Dispatch == nil || task.Dispatch.Pending || task.Dispatch.Candidate == nil {
				t.Fatalf("invalid repaired dispatch state: %+v", task.Dispatch)
			}
			candidate, _, err := manager.readDispatchCandidate(ctx, dispatchCandidateKey(*task.Dispatch.Candidate))
			if err != nil || !candidateMatches(task, candidate) {
				t.Fatalf("candidate was not rebuilt: %+v, %v", candidate, err)
			}
		})
	}
}

func TestDispatchCandidateTracksNewRoundAndTerminalCancellation(t *testing.T) {
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
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "round", "sha256:"+strings.Repeat("b", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	old := *task.Dispatch.Candidate
	task.RoundID = "new-round"
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if candidateMatches(task, old) || task.Dispatch.Candidate == nil || !candidateMatches(task, *task.Dispatch.Candidate) {
		t.Fatal("new round did not replace the old candidate")
	}
	current := *task.Dispatch.Candidate
	task.Status = Cancelled
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if candidateMatches(task, current) || task.Dispatch.Candidate != nil {
		t.Fatal("terminal cancellation retained an executable candidate")
	}
	if _, err := store.Head(ctx, dispatchCandidateKey(current)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancelled candidate remains: %v", err)
	}
}
