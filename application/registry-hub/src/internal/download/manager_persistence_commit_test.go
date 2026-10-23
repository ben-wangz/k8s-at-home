package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

func TestSlowNormalEOFFlushUsesTransferContext(t *testing.T) {
	_, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing, slowStateOffset: 3, stateDelay: 5200 * time.Millisecond}
	manager := New(store, 1<<20)
	manager.chunkSize = 4
	content := []byte("abc")
	digest := artifact.CalculateSHA256(content)
	request := testRequest(content, digest, 41, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	request.FinalizeTimeout = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	started := time.Now()
	result, err := manager.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Second || elapsed >= 15*time.Second {
		t.Fatalf("Fetch() elapsed %s, want the delayed write to finish between 5s and 15s", elapsed)
	}
	if result.Size != int64(len(content)) {
		t.Fatalf("Fetch() size = %d, want %d", result.Size, len(content))
	}
}

func TestPersistenceFailureVetoesRetryAndCommittedStateRecovers(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing, failStateOffset: 3, failStateMode: "error"}
	manager := New(store, 1<<20)
	manager.chunkSize = 4
	content := []byte("abc")
	digest := artifact.CalculateSHA256(content)
	calls := 0
	request := testRequest(content, digest, 42, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		calls++
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	request.Retryable = func(error) bool { return true }
	request.WaitRetry = func(context.Context, string, int) error { return errors.New("unexpected upstream wait") }

	if _, err := manager.Fetch(ctx, request); err == nil {
		t.Fatal("Fetch() succeeded after a failed state commit")
	} else {
		var persistenceErr *PersistenceError
		if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Fetch() error = %v, want typed storage timeout", err)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	stateData, _, err := manager.readState(ctx, stateKey(request.Identity, digest))
	if err != nil {
		t.Fatalf("read committed state: %v", err)
	}
	committed, err := decodeState(stateData, request)
	if err != nil || committed.Offset != 0 || committed.Index != "" {
		t.Fatalf("failed candidate leaked into state: offset=%d index=%q error=%v", committed.Offset, committed.Index, err)
	}

	manager = New(store, 1<<20)
	manager.chunkSize = 4
	request.OwnerFence++
	request.Source = func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	}
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("independent Fetch() did not recover the durable prefix: %v", err)
	}
	body, size, err := manager.Open(ctx, request.Identity, digest, request.MaxBytes)
	if err != nil {
		t.Fatalf("Open() after recovery: %v", err)
	}
	actual, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || size != int64(len(content)) || !bytes.Equal(actual, content) {
		t.Fatalf("recovered blob=%q size=%d error=%v", actual, size, readErr)
	}
}

func TestUnknownStateCommitRequiresExactReadback(t *testing.T) {
	for _, mode := range []string{"applied", "not-applied"} {
		t.Run(mode, func(t *testing.T) {
			ctx, backing := testDownloadStore(t)
			store := &downloadFaultStore{Store: backing, failStateOffset: 3, failStateMode: mode}
			manager := New(store, 1<<20)
			manager.chunkSize = 4
			content := []byte("abc")
			digest := artifact.CalculateSHA256(content)
			calls := 0
			request := testRequest(content, digest, 43, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
				calls++
				return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
			})
			request.Retryable = func(error) bool { return true }
			_, err := manager.Fetch(ctx, request)
			if mode == "applied" {
				if err != nil {
					t.Fatalf("Fetch() after confirmed lost acknowledgment: %v", err)
				}
			} else {
				var persistenceErr *PersistenceError
				if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrUnknownCommit) {
					t.Fatalf("Fetch() error = %v, want unresolved persistence result", err)
				}
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
		})
	}
}

func TestQuotaUnknownCommitUsesReadbackWithoutRetry(t *testing.T) {
	for _, mode := range []string{"applied", "not-applied"} {
		t.Run(mode, func(t *testing.T) {
			ctx, backing := testDownloadStore(t)
			store := &downloadFaultStore{Store: backing, quotaFailureMode: mode}
			manager := New(store, 1024)
			request := Request{Identity: "registry/quota-unknown", Digest: "sha256:quota-unknown"}
			err := manager.reserve(ctx, request, "session", 50)
			if mode == "applied" {
				if err != nil {
					t.Fatalf("reserve after confirmed lost acknowledgment: %v", err)
				}
				if used, sessions, usageErr := manager.Usage(ctx); usageErr != nil || used != 50 || sessions != 1 {
					t.Fatalf("confirmed reservation usage=(%d,%d,%v)", used, sessions, usageErr)
				}
			} else {
				var persistenceErr *PersistenceError
				if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrUnknownCommit) {
					t.Fatalf("reserve error = %v, want unresolved commit", err)
				}
			}
			if got := store.putCount(quotaKey); got != 1 {
				t.Fatalf("quota PUT count = %d, want no same-call retry", got)
			}
		})
	}
}
