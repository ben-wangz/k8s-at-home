package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
	"testing"
	"time"
)

func setTaskTestSchemaVersion(t *testing.T, store storage.Store, key string, version int) storage.Entry {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["schemaVersion"], err = json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func taskTestSchemaVersion(t *testing.T, store storage.Store, key string) int {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var record struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.NewDecoder(object.Body).Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record.SchemaVersion
}

func TestTaskAndInFlightIndexMigratePreviousSchema(t *testing.T) {
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
	task.RootDigest = "sha256:" + strings.Repeat("a", 64)
	task.MediaType = "application/vnd.oci.image.index.v1+json"
	task.Graph = map[string]GraphEntry{task.RootDigest: {Digest: task.RootDigest, MediaType: task.MediaType, Size: 31}}
	task.Checkpoints = map[string]string{task.RootDigest: "verified"}
	task.RetryCount = 2
	task.Status = RetryWait
	task.NextAttemptAt = time.Now().UTC().Add(time.Minute)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	requestKey := inFlightKey("ghcr.io", "team/image", "latest")
	taskStorageKey := taskKey(task.ID)
	inFlightStorageKey := "tasks/inflight/" + requestKey
	setTaskTestSchemaVersion(t, store, taskStorageKey, taskSchemaPrevious)
	setTaskTestSchemaVersion(t, store, inFlightStorageKey, inFlightSchemaPrevious)

	loaded, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != taskSchemaCurrent {
		t.Fatalf("loaded task schemaVersion = %d, want %d", loaded.SchemaVersion, taskSchemaCurrent)
	}
	if loaded.RootDigest != task.RootDigest || loaded.MediaType != task.MediaType || loaded.Graph[task.RootDigest].Size != 31 || loaded.Checkpoints[task.RootDigest] != "verified" || loaded.RetryCount != 2 || loaded.Status != RetryWait {
		t.Fatalf("migrated task did not preserve state/graph/checkpoint: %#v", loaded)
	}
	if _, created, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest"); err != nil || created {
		t.Fatalf("enqueue existing task created=%t err=%v, want existing task", created, err)
	}
	if version := taskTestSchemaVersion(t, store, taskStorageKey); version != taskSchemaCurrent {
		t.Fatalf("task schemaVersion = %d, want %d", version, taskSchemaCurrent)
	}
	if version := taskTestSchemaVersion(t, store, inFlightStorageKey); version != inFlightSchemaCurrent {
		t.Fatalf("in-flight schemaVersion = %d, want %d", version, inFlightSchemaCurrent)
	}
}

func TestConcurrentConditionalUpdatesToDifferentTaskStates(t *testing.T) {
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
	first, _, err := manager.Enqueue(ctx, "ghcr.io", "team/first", "latest")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.Enqueue(ctx, "ghcr.io", "team/second", "latest")
	if err != nil {
		t.Fatal(err)
	}
	firstDigest := "sha256:" + strings.Repeat("1", 64)
	secondDigest := "sha256:" + strings.Repeat("2", 64)
	first.Graph = map[string]GraphEntry{firstDigest: {Digest: firstDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 101}}
	second.Graph = map[string]GraphEntry{secondDigest: {Digest: secondDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 202}}
	if err := manager.save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := manager.save(ctx, &second); err != nil {
		t.Fatal(err)
	}
	first, err = manager.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = manager.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	update := func(task Task, retryCount int) {
		defer workers.Done()
		ready <- struct{}{}
		<-start
		task.RetryCount = retryCount
		task.Status = RetryWait
		task.NextAttemptAt = time.Now().UTC().Add(time.Minute)
		results <- manager.save(ctx, &task)
	}
	workers.Add(2)
	go update(first, 3)
	go update(second, 5)
	<-ready
	<-ready
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent conditional task update: %v", err)
		}
	}

	for _, expected := range []struct {
		id         string
		digest     string
		size       int64
		retryCount int
	}{
		{id: first.ID, digest: firstDigest, size: 101, retryCount: 3},
		{id: second.ID, digest: secondDigest, size: 202, retryCount: 5},
	} {
		persisted, err := manager.Get(ctx, expected.id)
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := persisted.Graph[expected.digest]
		if !ok || entry.Size != expected.size || persisted.RetryCount != expected.retryCount || persisted.Status != RetryWait {
			t.Errorf("task %s state/graph = %#v, want retry=%d and graph size=%d", expected.id, persisted, expected.retryCount, expected.size)
		}
	}
}

func TestRetryOnlyAllowsRetryableSolidificationFailure(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "org/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = Failed
	task.Phase = "solidification"
	task.RootDigest = "sha256:" + strings.Repeat("a", 64)
	task.Retryable = false
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Retry(ctx, task.ID); err == nil {
		t.Fatal("permanent failure was accepted for retry")
	}

	task = mustGetTask(t, manager, task.ID)
	task.Retryable = true
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	retried, err := manager.Retry(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != Pending || retried.Retryable {
		t.Fatalf("retried task = %#v, want pending and not retryable while running", retried)
	}
}
