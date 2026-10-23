package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestFetchResumesPersistedPrefixAfterRestart(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	var calls []int64
	_, err := manager.Fetch(firstCtx, testRequest(content, digest, 1, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		calls = append(calls, start)
		return testResponse(http.StatusOK, content, int64(len(content)), &cancelAfterRead{reader: bytes.NewReader(content[:4]), cancel: cancelFirst}), func() {}, nil
	}))
	cancelFirst()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Fetch() error = %v, want canceled context", err)
	}
	manager = testManager(store)
	request := testRequest(content, digest, 2, func(_ context.Context, start, end int64, etag string) (*http.Response, context.CancelFunc, error) {
		calls = append(calls, start)
		if start != 4 || end != 7 || etag != "\"blob-v1\"" {
			t.Fatalf("resume request = start:%d end:%d etag:%q", start, end, etag)
		}
		response := testResponse(http.StatusPartialContent, content[4:], 4, io.NopCloser(bytes.NewReader(content[4:])))
		response.Header.Set("Content-Range", "bytes 4-7/8")
		response.Header.Set("ETag", "\"blob-v1\"")
		response.Header.Set("Docker-Content-Digest", digest)
		return response, func() {}, nil
	})
	result, err := manager.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("resume Fetch() error = %v", err)
	}
	if result.Size != int64(len(content)) || result.ResumedFrom != 4 {
		t.Fatalf("Fetch() result = %#v", result)
	}
	body, size, err := manager.Open(ctx, request.Identity, digest, request.MaxBytes)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || size != int64(len(content)) || !bytes.Equal(data, content) || artifact.VerifyDigest(digest, data) != nil {
		t.Fatalf("recovered content = %q size=%d error=%v", data, size, readErr)
	}
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 4 {
		t.Fatalf("upstream offsets = %v", calls)
	}
	if err := manager.CommitComplete(ctx, request.Identity, digest, 2); err != nil {
		t.Fatalf("CommitComplete() error = %v", err)
	}
}

func TestRangeIgnored200ReplacesPrefixAndUsesSameResponse(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	calls := 0
	request := testRequest(content, digest, 3, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		calls++
		if start == 0 {
			return testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])}), func() {}, nil
		}
		if start != 4 {
			t.Fatalf("Range request started at %d, want 4", start)
		}
		return testResponse(http.StatusOK, content, 8, io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	result, err := manager.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if calls != 2 || result.Size != 8 || result.ResumedFrom != 0 {
		t.Fatalf("Fetch() calls=%d result=%#v", calls, result)
	}
	body, _, err := manager.Open(ctx, request.Identity, digest, request.MaxBytes)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(data, content) {
		t.Fatalf("complete content = %q, error = %v", data, readErr)
	}
}

func TestCorruptSavedChunkResetsToZero(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	request := testRequest(content, digest, 4, func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, 8, &cancelAfterRead{reader: bytes.NewReader(content[:4]), cancel: cancelFirst}), func() {}, nil
	})
	_, err := manager.Fetch(firstCtx, request)
	cancelFirst()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Fetch() error = %v", err)
	}
	entries, _, err := store.List(ctx, partialObjectPrefix(request.Identity, digest)+"sessions/", "", 100)
	if err != nil || len(entries) == 0 {
		t.Fatalf("stored chunks = %v, error = %v", entries, err)
	}
	var chunkEntry storage.Entry
	for _, entry := range entries {
		if bytes.Contains([]byte(entry.Key), []byte("/chunks/")) {
			chunkEntry = entry
			break
		}
	}
	if chunkEntry.Key == "" {
		t.Fatal("no persisted chunk found")
	}
	if _, err := store.Put(ctx, chunkEntry.Key, bytes.NewReader([]byte("xxxx")), 4, storage.PutOptions{IfMatch: chunkEntry.Version}); err != nil {
		t.Fatalf("corrupt persisted chunk: %v", err)
	}
	manager = testManager(store)
	start := int64(-1)
	request.OwnerFence = 5
	request.Source = func(_ context.Context, offset, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		start = offset
		return testResponse(http.StatusOK, content, 8, io.NopCloser(bytes.NewReader(content))), func() {}, nil
	}
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("Fetch() after corruption error = %v", err)
	}
	if start != 0 {
		t.Fatalf("recovery started at %d, want 0", start)
	}
}

func TestFetchRejectsUnsolicitedPartialAndQuotaOverflow(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	request := testRequest(content, digest, 6, func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		response := testResponse(http.StatusPartialContent, content, 8, io.NopCloser(bytes.NewReader(content)))
		response.Header.Set("Content-Range", "bytes 0-7/8")
		return response, func() {}, nil
	})
	if _, err := manager.Fetch(ctx, request); err == nil {
		t.Fatal("Fetch() accepted an unsolicited 206 response")
	}
	limited := New(store, 64)
	limited.chunkSize = 4
	request.Identity = "registry/quota-repository"
	request.Digest = artifact.CalculateSHA256([]byte("quota-object"))
	request.ExpectedSize = 40
	if _, err := limited.Fetch(ctx, request); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Fetch() quota error = %v, want ErrQuotaExceeded", err)
	}
}

func TestUsageReportsPartialDownloadReservations(t *testing.T) {
	ctx, store := testDownloadStore(t)
	manager := New(store, 1024)
	first := Request{Identity: "registry/one", Digest: "sha256:first"}
	second := Request{Identity: "registry/two", Digest: "sha256:second"}
	if err := manager.reserve(ctx, first, "session-one", 20); err != nil {
		t.Fatal(err)
	}
	if err := manager.reserve(ctx, second, "session-two", 30); err != nil {
		t.Fatal(err)
	}
	reserved, sessions, err := manager.Usage(ctx)
	if err != nil || reserved != 50 || sessions != 2 {
		t.Fatalf("Usage() = (%d, %d, %v), want (50, 2, nil)", reserved, sessions, err)
	}
	if err := manager.release(ctx, first.Identity, first.Digest, "session-one"); err != nil {
		t.Fatal(err)
	}
	reserved, sessions, err = manager.Usage(ctx)
	if err != nil || reserved != 30 || sessions != 1 {
		t.Fatalf("Usage() after release = (%d, %d, %v), want (30, 1, nil)", reserved, sessions, err)
	}
}

func testDownloadStore(t *testing.T) (context.Context, *filesystem.Store) {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return context.Background(), store
}

func testManager(store storage.Store) *Manager {
	manager := New(store, 1<<20)
	manager.chunkSize = 4
	return manager
}

func testRequest(content []byte, digest string, ownerFence uint64, source func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error)) Request {
	return Request{
		Identity: "registry/repository", Digest: digest, ExpectedSize: int64(len(content)), MaxBytes: 1 << 20,
		OwnerFence: ownerFence, Source: source, LifecycleContext: context.Background(),
		Guard: func(ctx context.Context) error { return ctx.Err() },
	}
}

func testResponse(status int, content []byte, length int64, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/octet-stream")
	header.Set("ETag", "\"blob-v1\"")
	header.Set("Docker-Content-Digest", artifact.CalculateSHA256(content))
	return &http.Response{StatusCode: status, Header: header, ContentLength: length, Body: body}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterRead) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	n, _ := r.reader.Read(buffer)
	r.cancel()
	return n, io.ErrUnexpectedEOF
}

func (*cancelAfterRead) Close() error { return nil }

type readThenError struct{ reader io.Reader }

func (r *readThenError) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (*readThenError) Close() error { return nil }
