package tasks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
)

func TestS3SDKAttemptsAndTaskBackoffRemainSeparateAcrossRestart(t *testing.T) {
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "test-key", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "",
		"AWS_RETRY_MODE": "standard", "AWS_MAX_ATTEMPTS": "3", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null",
	} {
		t.Setenv(key, value)
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			_, _ = io.Copy(io.Discard, r.Body)
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	ctx := context.Background()
	s3, err := s3store.New(ctx, config.S3{Endpoint: server.URL, Region: "us-east-1", Bucket: "registry-hub",
		AllowHTTP: true, UsePathStyle: true, MultipartThreshold: 64 << 20, PartSize: 5 << 20,
		Timeout: 10 * time.Second, MaxConnections: 2, ReplayDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	stateStore, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	processor := &s3RetryLayerProcessor{store: s3}
	manager, err := NewManager(stateStore, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest",
		"sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	manager.dispatch(ctx, 0)
	manager.wg.Wait()
	first, err := manager.Get(ctx, task.ID)
	if err != nil || first.Status != RetryWait || first.RetryCount != 1 || !first.NextAttemptAt.After(time.Now()) || attempts.Load() != 3 {
		t.Fatalf("first logical attempt = %#v, %v, HTTP attempts=%d; want one task retry and three SDK attempts", first, err, attempts.Load())
	}
	first.NextAttemptAt = time.Now().Add(time.Minute)
	if err := manager.save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	restarted, err := NewManager(stateStore, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if err := restarted.ensureWakeRequest(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileWakeRequests(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.dispatch(ctx, 0)
	restarted.wg.Wait()
	if attempts.Load() != 3 {
		t.Fatalf("restart or event bypassed task backoff: HTTP attempts=%d", attempts.Load())
	}
	first, err = restarted.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	first.NextAttemptAt = time.Now().Add(-time.Second)
	if err := restarted.save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	restarted.dispatch(ctx, 0)
	restarted.wg.Wait()
	second, err := restarted.Get(ctx, task.ID)
	if err != nil || second.Status != RetryWait || second.RetryCount != 2 || attempts.Load() != 6 {
		t.Fatalf("second logical attempt = %#v, %v, HTTP attempts=%d; want two task retries and six SDK attempts", second, err, attempts.Load())
	}
}

type s3RetryLayerProcessor struct{ store storage.Store }

func (p *s3RetryLayerProcessor) Process(ctx context.Context, _ *Task, _ lease.Lease) error {
	_, err := p.store.Put(ctx, "test/retry-layer", strings.NewReader("payload"), 7, storage.PutOptions{IfAbsent: true})
	if err != nil {
		return &Failure{Class: "storage", Err: err}
	}
	return nil
}
