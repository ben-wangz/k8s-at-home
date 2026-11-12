package s3store

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriedMultipartVisibilityHonorsCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := store.resolveRetriedMultipartCommit(ctx, "test/cancel", 1, nil, "operation")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("HEAD did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("visibility error = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("visibility check did not cancel its in-flight HEAD")
	}
}

func TestRetriedMultipartVisibilityRejectsUntrustedReadback(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		operation string
		body      string
	}{
		{name: "other operation", operation: "different", body: "valid"},
		{name: "corrupt payload", operation: "expected", body: "wrong"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("ETag", `"version"`)
				writer.Header().Set("Content-Length", "5")
				writer.Header().Set("x-amz-meta-registry-hub-operation", scenario.operation)
				if request.Method == http.MethodGet {
					gets.Add(1)
					_, _ = io.WriteString(writer, scenario.body)
				}
			}))
			defer server.Close()
			store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
			defer store.Close()
			digest := sha256.Sum256([]byte("valid"))
			_, matched, err := store.resolveRetriedMultipartCommit(context.Background(), "test/untrusted", 5, digest[:], "expected")
			if err != nil || matched {
				t.Fatalf("untrusted readback matched=%t error=%v, want rejection", matched, err)
			}
			if count := gets.Load(); count > 1 {
				t.Fatalf("payload reads = %d, want at most one", count)
			}
		})
	}
}

func TestRetriedMultipartVisibilityStopsOnPermissionDenied(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()
	_, _, err := store.resolveRetriedMultipartCommit(context.Background(), "test/denied", 1, nil, "operation")
	if !errors.Is(err, storage.ErrPermissionDenied) || calls.Load() != 1 {
		t.Fatalf("permission error=%v calls=%d, want immediate denial", err, calls.Load())
	}
}

func TestRetriedMultipartVisibilityHonorsItsOwnDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()
	before := time.Now()
	_, _, err := store.resolveRetriedMultipartCommitWithin(context.Background(), "test/deadline", 1, nil, "operation", 50*time.Millisecond)
	if !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("visibility error = %v, want storage timeout", err)
	}
	if elapsed := time.Since(before); elapsed > time.Second {
		t.Fatalf("visibility deadline took %s, want under one second", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("HEAD did not start")
	}
}
