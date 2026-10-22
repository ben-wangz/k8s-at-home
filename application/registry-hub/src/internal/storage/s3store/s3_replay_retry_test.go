package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
)

func TestPutObjectRetriesWithAnIdenticalReplayableBody(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var operationIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		var body []byte
		var err error
		mu.Lock()
		attempt := len(bodies) + 1
		mu.Unlock()
		if attempt == 1 {
			body, err = io.ReadAll(io.LimitReader(request.Body, 7))
		} else {
			body, err = io.ReadAll(request.Body)
		}
		if err != nil {
			http.Error(writer, "read failed", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		operationIDs = append(operationIDs, request.Header.Get("x-amz-meta-registry-hub-operation"))
		attempt = len(bodies)
		mu.Unlock()
		if attempt == 1 {
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		writer.Header().Set("ETag", `"put-etag"`)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	defer store.Close()

	data := []byte("retryable S3 PutObject payload")
	input := &replayCountingReader{reader: bytes.NewReader(data)}
	entry, err := store.Put(context.Background(), "test/replay-single", input, int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("Put after SDK retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"put-etag"` || len(bodies) != 2 {
		t.Fatalf("Put entry=%#v attempts=%d", entry, len(bodies))
	}
	if !bytes.Equal(bodies[0], data[:7]) || !bytes.Equal(bodies[1], data) {
		t.Fatalf("Put retry bodies = %q, %q; want a partial first request and the complete replay", bodies[0], bodies[1])
	}
	if input.bytesRead != len(data) {
		t.Fatalf("source bytes read = %d, want one pass over %d bytes", input.bytesRead, len(data))
	}
	if operationIDs[0] == "" || operationIDs[0] != operationIDs[1] {
		t.Fatalf("Put operation IDs = %q, %q", operationIDs[0], operationIDs[1])
	}
}

func TestMultipartPartRetryReusesUploadIDPartNumberAndBytes(t *testing.T) {
	var mu sync.Mutex
	var uploadIDs []string
	var partNumbers []string
	partBodies := map[string][][]byte{}
	acceptedParts := map[string][]byte{}
	var completedObject []byte
	var operationID string
	var completeCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			mu.Lock()
			operationID = request.Header.Get("x-amz-meta-registry-hub-operation")
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>retry-upload</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("uploadId") == "retry-upload":
			partNumber := request.URL.Query().Get("partNumber")
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			partBodies[partNumber] = append(partBodies[partNumber], body)
			uploadIDs = append(uploadIDs, request.URL.Query().Get("uploadId"))
			partNumbers = append(partNumbers, partNumber)
			attempt := len(partBodies[partNumber])
			mu.Unlock()
			if partNumber == "1" && attempt == 1 {
				writer.Header().Set("Content-Type", "application/xml")
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
				return
			}
			mu.Lock()
			acceptedParts[partNumber] = append([]byte(nil), body...)
			mu.Unlock()
			writer.Header().Set("ETag", `"part-`+partNumber+`-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "retry-upload":
			mu.Lock()
			completeCount++
			completedObject = append(completedObject, acceptedParts["1"]...)
			completedObject = append(completedObject, acceptedParts["2"]...)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<CompleteMultipartUploadResult><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()

	data := append(bytes.Repeat([]byte("x"), 5<<20), []byte("final-part")...)
	entry, err := store.Put(context.Background(), "test/replay-multipart", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("multipart Put after SDK retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"complete-etag"` || entry.Size != int64(len(data)) || len(partBodies["1"]) != 2 || len(partBodies["2"]) != 1 {
		t.Fatalf("multipart entry=%#v part attempts=%v", entry, partBodies)
	}
	firstPart := data[:5<<20]
	secondPart := data[5<<20:]
	if !bytes.Equal(partBodies["1"][0], firstPart) || !bytes.Equal(partBodies["1"][1], firstPart) || !bytes.Equal(partBodies["2"][0], secondPart) {
		t.Fatalf("multipart part bodies do not match the expected source ranges")
	}
	if operationID == "" || len(uploadIDs) != 3 || uploadIDs[0] != "retry-upload" || uploadIDs[1] != uploadIDs[0] || uploadIDs[2] != uploadIDs[0] ||
		len(partNumbers) != 3 || partNumbers[0] != "1" || partNumbers[1] != "1" || partNumbers[2] != "2" || completeCount != 1 {
		t.Fatalf("multipart identity operation=%q upload IDs=%v part numbers=%v completes=%d", operationID, uploadIDs, partNumbers, completeCount)
	}
	if !bytes.Equal(completedObject, data) {
		t.Fatalf("completed multipart bytes differ from source: got %d bytes, want %d", len(completedObject), len(data))
	}
	if sha256.Sum256(completedObject) != sha256.Sum256(data) {
		t.Fatal("completed multipart digest differs from source")
	}
}

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

func s3PutAttemptsForFailureContext(t *testing.T, ctx context.Context, cancelOnPut func()) int {
	t.Helper()
	var mu sync.Mutex
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, request.Body)
			mu.Lock()
			attempts++
			firstAttempt := attempts == 1
			mu.Unlock()
			if firstAttempt && cancelOnPut != nil {
				cancelOnPut()
			}
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
		case http.MethodHead:
			writer.WriteHeader(http.StatusNotFound)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	store, err := New(context.Background(), config.S3{
		Endpoint:           server.URL,
		Region:             "us-east-1",
		Bucket:             "registry-hub",
		AllowHTTP:          true,
		UsePathStyle:       true,
		MultipartThreshold: 64 << 20,
		PartSize:           5 << 20,
		Timeout:            10 * time.Second,
		MaxConnections:     2,
		ReplayDirectory:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create configured S3 store: %v", err)
	}
	defer store.Close()
	if _, err := store.Put(ctx, "test/retry-budget", strings.NewReader("payload"), int64(len("payload")), storage.PutOptions{}); err == nil {
		t.Fatal("PUT unexpectedly succeeded against the failing fixture")
	}
	mu.Lock()
	defer mu.Unlock()
	return attempts
}

func unsetS3Environment(t *testing.T, name string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func newReplayRetryTestStore(t *testing.T, endpoint string, multipartThreshold, partSize int64) *Store {
	t.Helper()
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		Retryer: func() aws.Retryer {
			return trackingRetryer{delegate: retry.NewStandard(func(options *retry.StandardOptions) {
				options.MaxAttempts = 2
				options.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})}
		},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	return &Store{client: client, bucket: "registry-hub", multipartThreshold: multipartThreshold, partSize: partSize}
}

func newProductionRetryTestStore(t *testing.T, endpoint string) *Store {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_RETRY_MODE", "standard")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	unsetS3Environment(t, "AWS_MAX_ATTEMPTS")

	store, err := New(context.Background(), config.S3{
		Endpoint:           endpoint,
		Region:             "us-east-1",
		Bucket:             "registry-hub",
		AllowHTTP:          strings.HasPrefix(endpoint, "http://"),
		UsePathStyle:       true,
		MultipartThreshold: 64 << 20,
		PartSize:           5 << 20,
		Timeout:            10 * time.Second,
		MaxConnections:     2,
		ReplayDirectory:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create configured S3 store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close configured S3 store: %v", err)
		}
	})
	return store
}

type replayCountingReader struct {
	reader    io.Reader
	bytesRead int
}

func (r *replayCountingReader) Read(data []byte) (int, error) {
	read, err := r.reader.Read(data)
	r.bytesRead += read
	return read, err
}
