package download

import (
	"context"
	"io"
	"net/http"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

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
