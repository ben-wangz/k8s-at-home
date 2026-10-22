package s3store

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"testing"
	"time"
)

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
