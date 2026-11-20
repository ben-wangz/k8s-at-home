package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func setAppTestSchemaVersion(t *testing.T, store storage.Store, key string, version int) storage.Entry {
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["schemaVersion"], err = json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestGenerationReadMigratesPreviousSchema(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	const authority, repository, reference = "ghcr.io", "team/image", "latest"
	previous := generationRecord{
		SchemaVersion: generationSchemaPrevious,
		Authority:     authority,
		Repository:    repository,
		Reference:     reference,
		Generation:    7,
		UpdatedAt:     time.Now().UTC(),
		Tombstone:     true,
	}
	data, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	key := generationKey(authority, repository, reference)
	if _, err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}

	loaded, err := service.generation(ctx, authority, repository, reference)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != generationSchemaCurrent || loaded.Generation != previous.Generation || !loaded.Tombstone {
		t.Fatalf("migrated generation = %#v, want current schema with preserved state", loaded)
	}
	object, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var persisted generationRecord
	if err := json.NewDecoder(object.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	if persisted.SchemaVersion != generationSchemaCurrent || persisted.Generation != previous.Generation || !persisted.Tombstone {
		t.Fatalf("persisted generation after migration = %#v", persisted)
	}
}

func TestGenerationRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	const authority, repository, reference = "ghcr.io", "team/image", "latest"
	if _, err := service.advanceGeneration(ctx, authority, repository, reference, false); err != nil {
		t.Fatal(err)
	}
	key := generationKey(authority, repository, reference)
	unsupported := setAppTestSchemaVersion(t, store, key, 99)
	if _, err := service.generation(ctx, authority, repository, reference); err == nil {
		t.Fatal("generation with an unsupported schema was accepted")
	}
	after, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != unsupported.Version {
		t.Fatal("generation with an unsupported schema was rewritten")
	}
}

func TestDeletionGenerationBlocksOldTaskAndSurvivesCacheCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := &Service{store: store}
	const authority, repository, reference = "ghcr.io", "team/image", "latest"
	created, err := service.advanceGeneration(ctx, authority, repository, reference, false)
	if err != nil {
		t.Fatalf("create generation: %v", err)
	}
	oldTask := &tasks.Task{Authority: authority, Repository: repository, Reference: reference, Generation: created.Generation}

	deleted, err := service.advanceGeneration(ctx, authority, repository, reference, true)
	if err != nil {
		t.Fatalf("advance deletion generation: %v", err)
	}
	if deleted.Generation != created.Generation+1 || !deleted.Tombstone {
		t.Fatalf("deletion generation = %#v, want incremented tombstone", deleted)
	}
	if err := service.checkTaskGeneration(ctx, oldTask); err == nil {
		t.Fatal("old task generation was accepted after deletion")
	}

	cache := artifact.NewCache(store)
	if _, err := cache.Cleanup(ctx, deleted.UpdatedAt.AddDate(1, 0, 0), 100); err != nil {
		t.Fatalf("cache cleanup: %v", err)
	}
	persisted, err := service.generation(ctx, authority, repository, reference)
	if err != nil {
		t.Fatalf("read generation after cleanup: %v", err)
	}
	if persisted.Generation != deleted.Generation || !persisted.Tombstone {
		t.Fatalf("generation after cleanup = %#v, want retained counter and tombstone", persisted)
	}
}

func TestFreshBlobDemandClearsDeletionTombstoneAndFencesOldTask(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	identity := tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: artifact.CalculateSHA256([]byte("deleted blob")),
	}
	initial, err := f.service.advanceGeneration(ctx, identity.Authority, identity.Repository, identity.Digest, false)
	if err != nil {
		t.Fatal(err)
	}
	oldIntent, _, err := f.service.records.EnsureBlobIntent(ctx, identity, initial.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	oldIntent, err = f.service.records.SetBlobIntentReady(ctx, oldIntent.ID, "application/octet-stream", "cache-v1", 12)
	if err != nil {
		t.Fatal(err)
	}
	oldTask, _, err := f.service.tasks.EnqueueBlob(ctx, oldIntent)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := f.service.advanceGeneration(ctx, identity.Authority, identity.Repository, identity.Digest, true)
	if err != nil {
		t.Fatal(err)
	}

	newIntent, err := f.service.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil {
		t.Fatalf("start fresh blob demand: %v", err)
	}
	current, err := f.service.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if current.Generation != deleted.Generation+1 || current.Tombstone {
		t.Fatalf("generation after fresh demand = %#v, want advanced and clear tombstone", current)
	}
	if newIntent.ID == oldIntent.ID || newIntent.Round != oldIntent.Round+1 || newIntent.Generation != current.Generation {
		t.Fatalf("new intent = %#v, want next round at current generation %d", newIntent, current.Generation)
	}

	oldLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+oldTask.ID, "stale-generation-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, oldLease)
	processor := &solidifier{service: f.service, tasks: f.service.tasks}
	err = processor.processBlob(ctx, &oldTask, oldLease)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "generation" || !failure.Permanent {
		t.Fatalf("stale blob worker result = %v, want permanent generation failure", err)
	}
	unchanged, err := f.service.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil {
		t.Fatalf("repeat blob demand: %v", err)
	}
	if unchanged.ID != newIntent.ID {
		t.Fatalf("repeat demand changed the active intent: %q -> %q", newIntent.ID, unchanged.ID)
	}
	latest, err := f.service.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil || latest.Generation != current.Generation {
		t.Fatalf("repeat demand changed generation: %#v err=%v", latest, err)
	}
}
