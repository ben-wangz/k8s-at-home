package inventory

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"sync"
	"testing"
	"time"
)

func testRepository(t *testing.T) *Repository {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Repository{Store: store}
}

type uncertainStore struct {
	storage.Store
	once sync.Once
}

func (s *uncertainStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err != nil {
		return entry, err
	}
	unknown := false
	s.once.Do(func() { unknown = true })
	if unknown {
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, nil
}

func TestLifecycleCASAndUnknownCommit(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	repo.Store = &uncertainStore{Store: repo.Store}
	if err := repo.Register(ctx, "legacy", false); err != nil {
		t.Fatal(err)
	}
	legacy, err := repo.Lifecycle(ctx, "legacy")
	if err != nil || legacy.Started != nil {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	if err := repo.Register(ctx, "new", true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.Start(ctx, "new"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	first, err := repo.Lifecycle(ctx, "new")
	if err != nil || first.Started == nil || !*first.Started || first.FirstStartedAt == nil {
		t.Fatalf("lifecycle=%+v err=%v", first, err)
	}
	if err := repo.Register(ctx, "new", true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Start(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	again, _ := repo.Lifecycle(ctx, "new")
	if !again.FirstStartedAt.Equal(*first.FirstStartedAt) {
		t.Fatal("start time changed on retry")
	}
}

func TestBindingRevisionAndSourceOrdering(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	id := Identity{Authority: "ghcr.io", Repository: "team/image", Reference: "latest"}
	now := time.Now().UTC()
	a, err := repo.Bind(ctx, Binding{Identity: id, Digest: "a", Source: "upstream", ConfirmedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.Bind(ctx, Binding{Identity: id, Digest: "b", Source: "upstream", ConfirmedAt: now.Add(time.Second)})
	if err != nil || a.Revision == b.Revision {
		t.Fatalf("revision not changed: %v", err)
	}
	old, err := repo.Bind(ctx, Binding{Identity: id, Digest: "a", Source: "distribution", ConfirmedAt: now.Add(time.Minute)})
	if err != nil || old.Digest != "b" {
		t.Fatalf("stale catalog overwrote upstream: %+v %v", old, err)
	}
	if id.ID() != b.Identity.ID() || LifecycleVersion(id.ID(), a.Revision, 0) == LifecycleVersion(id.ID(), b.Revision, 0) {
		t.Fatal("identity/version conflation")
	}
	if errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
}
