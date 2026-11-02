package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

type nestedMetadataStore struct {
	storage.Store
	calls  map[string]int
	writes map[string]int
}

func (s *nestedMetadataStore) record(operation, key string) {
	s.calls[operation]++
	if operation == "put" {
		s.writes[key]++
	}
}

func (s *nestedMetadataStore) Get(ctx context.Context, key string) (storage.Object, error) {
	s.record("get", key)
	return s.Store.Get(ctx, key)
}

func (s *nestedMetadataStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.record("put", key)
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *nestedMetadataStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	s.record("head", key)
	return s.Store.Head(ctx, key)
}

func (s *nestedMetadataStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.record("list", prefix)
	return s.Store.List(ctx, prefix, token, limit)
}

func TestNestedPublicationBoundsMetadataAndAvoidsColdScopeAdmission(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	counted := &nestedMetadataStore{Store: store, calls: map[string]int{}, writes: map[string]int{}}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counted)
	enableInventory(f)
	addNestedHistoricalTasks(t, f)
	body := []byte("nested-platform-config")
	blob := artifact.Descriptor{Digest: artifact.CalculateSHA256(body), Size: int64(len(body)), MediaType: testOCIConfigMediaType}
	var platforms []artifact.Descriptor
	for _, name := range []string{"linux-amd64", "linux-arm64", "windows-amd64", "freebsd-riscv64"} {
		platforms = append(platforms, f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
			Config: &blob, Layers: []artifact.Descriptor{}, Annotations: map[string]string{"platform": name}}))
	}
	nested := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &platforms})
	children := []artifact.Descriptor{nested}
	parent := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	children = []artifact.Descriptor{parent}
	root := f.root("nested", f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children}))
	counted.calls, counted.writes = map[string]int{}, map[string]int{}
	assertWaitingDeps(t, f.step(root.ID))
	for key := range counted.writes {
		if strings.HasPrefix(key, "system/solidification/") {
			t.Fatalf("cold dependency admitted a protection scope: %s", key)
		}
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 {
		t.Fatal("cold manifest retained protection")
	}
	f.cacheObject(body, testOCIConfigMediaType)
	assertWaitingDeps(t, f.step(root.ID))
	f.transfer(blob)
	counted.calls = map[string]int{}
	if err := f.step(root.ID); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, v := range counted.calls {
		total += v
	}
	t.Logf("final root metadata round trips=%d, operations=%v", total, counted.calls)
	if total > 1750 {
		t.Fatalf("nested publication made %d metadata round trips, want <=1750", total)
	}
	if published, ok := f.published("nested"); !ok || artifact.CalculateSHA256(published.body) != root.RootDigest {
		t.Fatal("root bytes were not published")
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 {
		t.Fatal("published closure retained protection")
	}
}

func addNestedHistoricalTasks(t *testing.T, f *stagedAcceptanceFixture) {
	t.Helper()
	ctx := context.Background()
	for i := range 20 {
		task, _, err := f.service.tasks.Enqueue(ctx, "ghcr.io", "history/image", fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		owned, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "history", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		task.Status = tasks.Solidified
		if err := f.service.tasks.SaveProgress(ctx, &task, owned); err != nil {
			t.Fatal(err)
		}
		if err := f.service.tasks.Leases().Release(ctx, owned); err != nil {
			t.Fatal(err)
		}
	}
}
