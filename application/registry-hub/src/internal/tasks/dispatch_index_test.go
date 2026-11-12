package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingDispatchStore struct {
	storage.Store
	fail bool
}

type replacingCandidateStore struct {
	storage.Store
	taskKey, candidateKey string
	replacement           []byte
	once                  sync.Once
}

func (s *replacingCandidateStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == s.taskKey {
		s.once.Do(func() {
			entry, err := s.Store.Head(ctx, s.candidateKey)
			if err == nil {
				_, _ = s.Store.Put(ctx, s.candidateKey, bytes.NewReader(s.replacement), int64(len(s.replacement)), storage.PutOptions{IfMatch: entry.Version})
			}
		})
	}
	return s.Store.Get(ctx, key)
}

func (s *failingDispatchStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.fail && strings.HasPrefix(key, dispatchPrefix+"ready/") {
		return storage.Entry{}, storage.ErrUnavailable
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestDispatchIndexRepairsFailedPublicationAndMissingCandidate(t *testing.T) {
	raw, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	store := &failingDispatchStore{Store: raw, fail: true}
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	if task.Dispatch == nil || !task.Dispatch.Pending || task.Dispatch.Candidate == nil {
		t.Fatalf("failed index publication did not remain pending: %+v", task.Dispatch)
	}
	store.fail = false
	if err := manager.repairDispatchTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if task.Dispatch.Pending {
		t.Fatal("successful publication was not acknowledged")
	}
	key := dispatchCandidateKey(*task.Dispatch.Candidate)
	if err := raw.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := manager.repairDispatchTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Head(ctx, key); err != nil {
		t.Fatalf("missing candidate not rebuilt: %v", err)
	}
}

func TestDispatchCandidateRejectsStaleRevisionAndFutureRetry(t *testing.T) {
	raw, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	manager, err := NewManager(raw, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("b", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	old := *task.Dispatch.Candidate
	task.Status, task.NextAttemptAt = RetryWait, time.Now().Add(time.Hour)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if candidateMatches(task, old) || dispatchEligible(task, time.Now()) {
		t.Fatal("old runnable candidate or future retry became executable")
	}
	if _, err := raw.Head(ctx, dispatchCandidateKey(old)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old candidate remains: %v", err)
	}
	if err := manager.promoteDispatchRetries(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Get(ctx, task.ID)
	if err != nil || current.Status != RetryWait {
		t.Fatalf("future retry was promoted: %+v %v", current, err)
	}
}

func TestDueRetryPromotionPreservesCanonicalState(t *testing.T) {
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
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("c", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.NextAttemptAt = RetryWait, time.Now().UTC().Add(-time.Second)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	candidate := *task.Dispatch.Candidate
	if err := manager.promoteDispatchRetries(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Get(ctx, task.ID)
	if err != nil || current.Status != RetryWait || !current.NextAttemptAt.Equal(task.NextAttemptAt) {
		t.Fatalf("retry state changed during promotion: %+v, %v", current, err)
	}
	if _, err := store.Head(ctx, readyDispatchCandidateKey(candidate)); err != nil {
		t.Fatalf("due candidate not promoted: %v", err)
	}
	if err := manager.repairDispatchTask(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, dispatchCandidateKey(candidate)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("repair recreated delayed candidate: %v", err)
	}
}

func TestDispatchPublicationReplaysAfterConcurrentWake(t *testing.T) {
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
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("d", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.WaitReason = WaitingCache, "cache_entry_unavailable"
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	holder, err := manager.leases.Acquire(ctx, "dispatch-index/"+task.ID, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := manager.Wake(ctx, task.ID); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		current, err := manager.Get(ctx, task.ID)
		if err == nil && current.Status == Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wake did not commit while index lease was held")
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.leases.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := manager.Get(ctx, task.ID)
	if err != nil || current.Dispatch.Pending {
		t.Fatalf("wake index update not acknowledged: %+v, %v", current.Dispatch, err)
	}
	if _, err := store.Head(ctx, dispatchCandidateKey(*current.Dispatch.Candidate)); err != nil {
		t.Fatalf("wake candidate missing: %v", err)
	}
}

func TestStaleDispatchConsumerCannotDeleteReplacedCandidate(t *testing.T) {
	raw, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
	manager, err := NewManager(raw, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); manager.Close() }()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "latest", "sha256:"+strings.Repeat("e", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	candidate := *task.Dispatch.Candidate
	key := dispatchCandidateKey(candidate)
	valid, _ := json.Marshal(candidate)
	candidate.Revision = "stale"
	stale, _ := json.Marshal(candidate)
	entry, err := raw.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Put(ctx, key, bytes.NewReader(stale), int64(len(stale)), storage.PutOptions{IfMatch: entry.Version}); err != nil {
		t.Fatal(err)
	}
	manager.store = &replacingCandidateStore{Store: raw, taskKey: taskKey(task.ID), candidateKey: key, replacement: valid}
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Head(ctx, key); err != nil {
		t.Fatalf("stale consumer removed replacement: %v", err)
	}
}
