package inventory

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

type assemblyReads struct {
	storage.Store
	key   string
	reads int
}

func (s *assemblyReads) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == s.key {
		s.reads++
	}
	return s.Store.Get(ctx, key)
}

func TestSnapshotAssemblyUsesOneCheckpointReadPerResumedUnit(t *testing.T) {
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	ctx := context.Background()
	if err := writer.AddImage(ctx, Image{ID: ID("image"), Scope: "reference", Category: "unknown"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AssembleStep(ctx, 2); err != nil {
		t.Fatal(err)
	}
	spy := &assemblyReads{Store: repo.Store, key: writer.key("assembly.json")}
	repo.Store = spy
	for step := 0; step < 30; step++ {
		before := spy.reads
		done, err := repo.ResumeWriter(writer.Snapshot).AssembleStep(ctx, 2)
		if err != nil || spy.reads-before != 1 {
			t.Fatalf("assembly metadata read budget exceeded: reads=%d error=%v", spy.reads-before, err)
		}
		if done {
			return
		}
	}
	t.Fatal("assembly did not finish within bounded units")
}

func TestSnapshotAssemblyStaleCheckpointCannotReplaceNewerProgress(t *testing.T) {
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	ctx := context.Background()
	if _, err := writer.AssembleStep(ctx, 2); err != nil {
		t.Fatal(err)
	}
	key := writer.key("assembly.json")
	stale, version, err := Read[assemblyState](ctx, repo.Store, key)
	if err != nil {
		t.Fatal(err)
	}
	current := stale
	current.Revision++
	if err := writer.saveAssemblyStep(ctx, key, version, current); err != nil {
		t.Fatal(err)
	}
	stale.Revision += 2
	if err := writer.saveAssemblyStep(ctx, key, version, stale); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("stale checkpoint returned %v", err)
	}
	actual, _, err := Read[assemblyState](ctx, repo.Store, key)
	if err != nil || actual.Revision != current.Revision {
		t.Fatalf("newer progress was replaced: revision=%d error=%v", actual.Revision, err)
	}
}
