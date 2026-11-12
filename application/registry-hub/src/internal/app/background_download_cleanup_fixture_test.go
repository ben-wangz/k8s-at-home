package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type downloadAttemptStore struct {
	*filesystem.Store
	readBytes    atomic.Int64
	checkpoints  int
	beforeSave   func() error
	afterSave    func()
	beforeDelete func(context.Context) error
	verifiedCtx  context.Context
}

type downloadAttemptReader struct {
	storage.PayloadReader
	count *atomic.Int64
}

func (r downloadAttemptReader) Read(p []byte) (int, error) {
	n, err := r.PayloadReader.Read(p)
	r.count.Add(int64(n))
	return n, err
}

func (s *downloadAttemptStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	s.verifiedCtx = ctx
	payload, err := s.Store.OpenPayload(ctx, key)
	if err == nil {
		payload.Body = downloadAttemptReader{PayloadReader: payload.Body, count: &s.readBytes}
	}
	return payload, err
}

func (s *downloadAttemptStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	checkpoint := false
	if strings.HasPrefix(key, workqueue.Prefix+"jobs/download_cleanup/") {
		data, err := io.ReadAll(body)
		if err != nil {
			return storage.Entry{}, err
		}
		body = bytes.NewReader(data)
		var job workqueue.Job
		checkpoint = json.Unmarshal(data, &job) == nil && job.State == "running" && len(job.Progress) > 0
	}
	if checkpoint && s.beforeSave != nil {
		if err := s.beforeSave(); err != nil {
			return storage.Entry{}, err
		}
	}
	entry, err := s.Store.Put(ctx, key, body, size, opts)
	if err == nil && checkpoint {
		s.checkpoints++
		if s.afterSave != nil {
			s.afterSave()
		}
	}
	return entry, err
}

func (s *downloadAttemptStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if s.beforeDelete != nil && strings.Contains(key, "/sessions/") && strings.HasPrefix(key, "downloads/v1/") {
		if err := s.beforeDelete(ctx); err != nil {
			return err
		}
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

type downloadAttemptFixture struct {
	service *Service
	store   *downloadAttemptStore
	job     workqueue.Job
	source  downloadCleanupWork
	size    int64
	prefix  string
}

func newDownloadAttemptFixture(t *testing.T, extra int) downloadAttemptFixture {
	t.Helper()
	ctx := context.Background()
	s, _, _ := newDiscoveryTestService(t, &scriptedUpstream{})
	store := &downloadAttemptStore{Store: s.store.(*filesystem.Store)}
	s.store, s.cache = store, artifact.NewCache(store)
	data := bytes.Repeat([]byte("attempt"), 4096)
	size := int64(len(data))
	digest := artifact.CalculateSHA256(data)
	identity := objectLeaseKey("ghcr.io", "team/image", digest)
	_, err := s.downloadManager().Fetch(ctx, download.Request{
		Identity: identity, Digest: digest, ExpectedSize: size, MaxBytes: size, OwnerFence: 1,
		Guard: func(ctx context.Context) error { return ctx.Err() },
		Source: func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
			return &http.Response{StatusCode: http.StatusOK, ContentLength: size, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(data))}, func() {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.downloadManager().CompletedTicket(ctx, identity, digest, 1)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := storage.Scan(ctx, store, "downloads/v1/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	prefix := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Key, "/state.json") {
			prefix = strings.TrimSuffix(entry.Key, "state.json")
		}
	}
	if prefix == "" {
		t.Fatal("download state missing")
	}
	for i := range extra {
		key := fmt.Sprintf("%ssessions/%s/test/%06d", prefix, ticket.Session, i)
		if _, err := store.Put(ctx, key, strings.NewReader("chunk metadata"), 14, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	cached, err := s.cache.PutCheckedRepresentation(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(data), size, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := downloadCleanupWork{CleanupTicket: ticket, Authority: "ghcr.io", Repository: "team/image", CacheVersion: cached.DataVersion}
	job, err := s.backgroundQueue().Enqueue(ctx, "download_cleanup", "attempt", source, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return downloadAttemptFixture{service: s, store: store, job: job, source: source, size: size, prefix: prefix}
}

func (f *downloadAttemptFixture) claim(t *testing.T) {
	t.Helper()
	var err error
	f.job, err = f.service.backgroundQueue().ClaimFor(context.Background(), f.job, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *downloadAttemptFixture) assertReleased(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pins, _, err := storage.Scan(ctx, f.store, "cache/readers/", "", 100)
	if err != nil || len(pins) != 0 {
		t.Fatalf("leaked pins=%v, error=%v", pins, err)
	}
	l, err := f.service.tasks.Leases().Acquire(ctx, "objects/"+f.source.Identity, "test-release", objectLeaseTTL)
	if err != nil {
		t.Fatalf("object lease retained: %v", err)
	}
	_ = f.service.tasks.Leases().Release(ctx, l)
}
