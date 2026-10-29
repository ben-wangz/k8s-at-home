package s3store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sync"
	"testing"
	"time"
)

type inventoryS3Fixture struct {
	mu        sync.Mutex
	objects   map[string][]byte
	failReads bool
}

func (f *inventoryS3Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, exists := f.objects[r.URL.Path]
	etag := func(body []byte) string { sum := sha256.Sum256(body); return `"` + hex.EncodeToString(sum[:]) + `"` }
	if f.failReads && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r, etag)
		return
	}
	if r.Method == http.MethodPut {
		if r.Header.Get("If-None-Match") == "*" && exists || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != etag(data)) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(412)
			_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", 500)
			return
		}
		f.objects[r.URL.Path] = body
		w.Header().Set("ETag", etag(body))
		return
	}
	if r.Method == http.MethodDelete {
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !exists {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	w.Header().Set("ETag", etag(data))
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

type inventoryUnknownCommitStore struct {
	storage.Store
	once sync.Once
}

func (s *inventoryUnknownCommitStore) Put(ctx context.Context, key string, r io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, r, size, opts)
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

func TestInventoryLifecycleAndSnapshotsThroughS3ConditionalAdapter(t *testing.T) {
	fixture := &inventoryS3Fixture{objects: map[string][]byte{}}
	server := httptest.NewServer(fixture)
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 64<<20, 5<<20)
	defer store.Close()
	repo := &inventory.Repository{Store: &inventoryUnknownCommitStore{Store: store}}
	ctx := context.Background()
	if err := repo.Register(ctx, "scope-version", true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Start(ctx, "scope-version"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := repo.Lifecycle(ctx, "scope-version")
	if err != nil || lifecycle.Started == nil || !*lifecycle.Started {
		t.Fatalf("lifecycle=%+v err=%v", lifecycle, err)
	}
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	for n := 0; n < 3; n++ {
		image := inventory.Image{ID: inventory.ID("image", fmt.Sprint(n)), Scope: "reference", Category: "unknown"}
		if err := writer.AddImage(ctx, image, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for done := false; !done; {
		var err error
		done, err = writer.Assemble(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	query := inventory.Query{Kind: "images", Scope: "reference", Limit: 2, Filters: url.Values{}}
	first, err := repo.Images(ctx, query, []byte("key"), time.Minute)
	if err != nil || len(first.Items) != 2 || first.NextToken == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	query.Token = first.NextToken
	last, err := repo.Images(ctx, query, []byte("key"), time.Minute)
	if err != nil || len(last.Items) != 1 || last.SnapshotID != first.SnapshotID {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	fixture.mu.Lock()
	fixture.failReads = true
	fixture.mu.Unlock()
	if _, err := repo.Snapshot(ctx, "", time.Minute); err == nil {
		t.Fatal("S3 read failure became an empty inventory")
	}
}
