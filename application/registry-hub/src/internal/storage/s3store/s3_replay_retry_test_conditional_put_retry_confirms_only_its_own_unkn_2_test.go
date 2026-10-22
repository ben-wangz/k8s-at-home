package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConditionalPutRetryConfirmsOnlyItsOwnUnknownCommit(t *testing.T) {
	var mu sync.Mutex
	var committed, operationID []byte
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPut:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			attempts++
			if attempts == 1 {
				committed = append([]byte(nil), body...)
				operationID = []byte(request.Header.Get("x-amz-meta-registry-hub-operation"))
				mu.Unlock()
				writer.Header().Set("Content-Type", "application/xml")
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
				return
			}
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
		case http.MethodHead:
			mu.Lock()
			length := len(committed)
			id := string(operationID)
			mu.Unlock()
			if length == 0 {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("Content-Length", fmt.Sprint(length))
			writer.Header().Set("x-amz-meta-registry-hub-operation", id)
		case http.MethodGet:
			mu.Lock()
			body := append([]byte(nil), committed...)
			id := string(operationID)
			mu.Unlock()
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("x-amz-meta-registry-hub-operation", id)
			_, _ = writer.Write(body)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	defer store.Close()

	data := []byte("commit persisted but response was lost")
	entry, err := store.Put(context.Background(), "test/replay-unknown", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("Put after verified unknown commit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"committed-etag"` || entry.Size != int64(len(data)) || attempts != 2 || !bytes.Equal(committed, data) || len(operationID) == 0 {
		t.Fatalf("confirmed entry=%#v attempts=%d committed=%q operation=%q", entry, attempts, committed, operationID)
	}
}

func TestNewUsesStandardS3AttemptsAndEnvironmentOverride(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_RETRY_MODE", "standard")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	unsetS3Environment(t, "AWS_MAX_ATTEMPTS")

	if attempts := s3PutAttemptsForFailure(t); attempts != 3 {
		t.Fatalf("default S3 PUT attempts = %d, want 3", attempts)
	}
	t.Setenv("AWS_MAX_ATTEMPTS", "2")
	if attempts := s3PutAttemptsForFailure(t); attempts != 2 {
		t.Fatalf("overridden S3 PUT attempts = %d, want 2", attempts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if attempts := s3PutAttemptsForFailureContext(t, ctx, cancel); attempts != 1 {
		t.Fatalf("cancelled S3 PUT attempts = %d, want one attempt", attempts)
	}
}

func TestNewUsesJitteredStandardBackoffWithTwentySecondCap(t *testing.T) {
	store := newProductionRetryTestStore(t, "https://s3.us-east-1.amazonaws.com")

	configured, ok := store.client.Options().Retryer.(trackingRetryer)
	if !ok {
		t.Fatalf("configured retryer has type %T, want trackingRetryer", store.client.Options().Retryer)
	}
	delays := make(map[time.Duration]struct{})
	for range 64 {
		delay, err := configured.RetryDelay(1, nil)
		if err != nil {
			t.Fatalf("first retry delay: %v", err)
		}
		if delay < 0 || delay >= 2*time.Second {
			t.Fatalf("first retry delay = %s, want jitter in [0s, 2s)", delay)
		}
		delays[delay] = struct{}{}
	}
	if len(delays) < 2 {
		t.Fatalf("first retry delay did not vary across samples: %v", delays)
	}

	delay, err := configured.RetryDelay(10, nil)
	if err != nil {
		t.Fatalf("capped retry delay: %v", err)
	}
	if delay < 0 || delay > 20*time.Second {
		t.Fatalf("retry delay at attempt 10 = %s, want no more than 20s", delay)
	}
}

func TestNewRetryObserverMarksConditionalConflictAfterRetryAsUnknownCommit(t *testing.T) {
	var mu sync.Mutex
	var putAttempts, headAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPut:
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			putAttempts++
			attempt := putAttempts
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			if attempt == 1 {
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
				return
			}
			writer.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
		case http.MethodHead:
			mu.Lock()
			headAttempts++
			mu.Unlock()
			writer.Header().Set("ETag", `"other-operation"`)
			writer.Header().Set("Content-Length", "7")
			writer.Header().Set("x-amz-meta-registry-hub-operation", "another-writer")
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	store := newProductionRetryTestStore(t, server.URL)

	_, err := store.Put(context.Background(), "test/unknown-after-retry", strings.NewReader("payload"), 7, storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrUnknownCommit) {
		t.Fatalf("Put error = %v, want ErrUnknownCommit after a retried conditional conflict", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if putAttempts != 2 || headAttempts != 1 {
		t.Fatalf("PUT attempts = %d, HEAD attempts = %d; want 2 and 1", putAttempts, headAttempts)
	}
}

func s3PutAttemptsForFailure(t *testing.T) int {
	return s3PutAttemptsForFailureContext(t, context.Background(), nil)
}
