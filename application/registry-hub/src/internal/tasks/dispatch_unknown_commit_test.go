package tasks

import (
	"bytes"
	"context"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

type unknownDispatchCommitStore struct {
	storage.Store
	mode  string
	fired bool
}

func (s *unknownDispatchCommitStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), size, options)
	if err != nil || s.fired {
		return entry, err
	}
	candidate := s.mode == "candidate" && strings.HasPrefix(key, dispatchPrefix+"ready/")
	ack := s.mode == "ack" && strings.HasPrefix(key, "tasks/state/") && bytes.Contains(data, []byte(`"pending":false`))
	if candidate || ack {
		s.fired = true
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, nil
}

func TestDispatchRepairsUnknownCandidateAndAcknowledgementCommits(t *testing.T) {
	for _, mode := range []string{"candidate", "ack"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			store := &unknownDispatchCommitStore{Store: raw, mode: mode}
			manager, err := NewManager(store, 1, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			ctx := context.Background()
			task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("f", 64), "application/vnd.oci.image.manifest.v1+json", 0)
			if err != nil || !store.fired {
				t.Fatalf("enqueue with unknown %s commit: fired=%t err=%v", mode, store.fired, err)
			}
			current, err := manager.Get(ctx, task.ID)
			if err != nil || current.Dispatch == nil || current.Dispatch.Candidate == nil {
				t.Fatalf("authoritative dispatch state missing: %+v, %v", current.Dispatch, err)
			}
			if err := manager.repairDispatchTask(ctx, &current); err != nil {
				t.Fatal(err)
			}
			if current.Dispatch.Pending {
				t.Fatal("dispatch acknowledgement remained pending")
			}
			candidate, _, err := manager.readDispatchCandidate(ctx, dispatchCandidateKey(*current.Dispatch.Candidate))
			if err != nil || !candidateMatches(current, candidate) {
				t.Fatalf("repaired candidate does not match the task: %+v, %v", candidate, err)
			}
		})
	}
}
