package tasks

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

type noopMetadataStore struct {
	storage.Store
	gets, heads, puts int
	unknownCommit     bool
}

func (s *noopMetadataStore) Get(ctx context.Context, key string) (storage.Object, error) {
	s.gets++
	return s.Store.Get(ctx, key)
}

func (s *noopMetadataStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	s.heads++
	return s.Store.Head(ctx, key)
}

func (s *noopMetadataStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.puts++
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && s.unknownCommit {
		s.unknownCommit = false
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, err
}

func TestCacheProtectionReferenceUsesValidatedGETAndRepairsMissingIndex(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &noopMetadataStore{Store: base, unknownCommit: true}
	records := NewRecords(store)
	taskID, digest := strings.Repeat("a", 32), "sha256:"+strings.Repeat("b", 64)
	ensure := func() error {
		return records.EnsureCacheProtectionReference(ctx, taskID, "ghcr.io", "team/image", digest)
	}
	if err := ensure(); err != nil {
		t.Fatalf("adopt uncertain create: %v", err)
	}
	store.gets, store.heads, store.puts = 0, 0, 0
	if err := ensure(); err != nil {
		t.Fatal(err)
	}
	if store.gets != 2 || store.heads != 0 || store.puts != 0 {
		t.Fatalf("existing reference: GET=%d HEAD=%d PUT=%d", store.gets, store.heads, store.puts)
	}
	hash, err := recordHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	key := cacheProtectionDigestIndexKey(hash, taskID)
	if err := base.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := ensure(); err != nil {
		t.Fatal(err)
	}
	if store.puts != 1 {
		t.Fatalf("missing reverse index writes=%d, want 1", store.puts)
	}
	conflict := `{"schemaVersion":1,"taskID":"conflict"}`
	if _, err := base.Put(ctx, key, strings.NewReader(conflict), int64(len(conflict)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := ensure(); err == nil {
		t.Fatal("conflicting protection identity was accepted")
	}
}

func TestUnchangedScopedDemandRepairsRootIndexWithoutRewritingDemand(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &noopMetadataStore{Store: base}
	records := NewRecords(store)
	intent, _, err := records.EnsureBlobIntent(ctx, BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64)}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	root, demand, session := strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	link := func(size int64) error {
		return records.LinkBlobDemandScope(ctx, intent.ID, demand, root, "scope", session, size)
	}
	if err := link(64); err != nil {
		t.Fatal(err)
	}
	before, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.puts = 0
	if err := link(64); err != nil {
		t.Fatal(err)
	}
	if store.puts != 0 {
		t.Fatalf("unchanged scoped demand rewrote %d records", store.puts)
	}
	if err := base.Delete(ctx, rootBlobDemandIndexKey(root, intent.ID, demand)); err != nil {
		t.Fatal(err)
	}
	if err := link(64); err != nil {
		t.Fatal(err)
	}
	if store.puts != 1 {
		t.Fatalf("root index repair writes=%d, want 1", store.puts)
	}
	after, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DemandRevision != before.DemandRevision || !after.LastProgressAt.Equal(before.LastProgressAt) {
		t.Fatal("replayed demand advanced idle or wake progress")
	}
	if err := link(65); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("conflicting scope size=%v", err)
	}
}
