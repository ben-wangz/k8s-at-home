package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/download"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestForegroundDownloadPersistenceFailureUsesStorageResponse(t *testing.T) {
	const body = "blob whose final state commit fails"
	const secret = "https://cdn.example/blob?token=must-not-leak"
	digest := artifact.CalculateSHA256([]byte(body))
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: body, contentType: "application/octet-stream"}}}
	service, _, _ := newDiscoveryTestService(t, remote)
	service.cfg.Limits = downloadTestLimits()
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.store = &stateCommitFailureStore{Store: service.store, message: secret}
	service.cache = artifact.NewCache(service.store)
	routed, err := route.Parse("/v2/ghcr.io/team/image/blobs/" + digest)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	response := httptest.NewRecorder()
	meta := &requestMeta{requestID: "request-storage-failure"}
	service.serveUpstreamBlob(response, request, routed, meta)

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" || meta.errorClass != "storage" {
		t.Fatalf("response status=%d retry-after=%q class=%q body=%q", response.Code, response.Header().Get("Retry-After"), meta.errorClass, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || len(remote.calls) != 1 {
		t.Fatalf("response leaked a storage error or retried upstream: body=%q calls=%d", response.Body.String(), len(remote.calls))
	}
	if !strings.Contains(logs.String(), "request-storage-failure") || !strings.Contains(logs.String(), "state_commit") || strings.Contains(logs.String(), secret) {
		t.Fatalf("download diagnostic is missing safe correlation or leaked the underlying error: %s", logs.String())
	}
}

func TestSolidifierDownloadPersistenceFailureIsTemporaryStorage(t *testing.T) {
	ctx := context.Background()
	const body = "background blob whose state commit fails"
	const secret = "https://cdn.example/blob?token=must-not-leak"
	digest := artifact.CalculateSHA256([]byte(body))
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: body, contentType: "application/octet-stream"}}}
	service, manager, _ := newDiscoveryTestService(t, remote)
	service.cfg.Limits = downloadTestLimits()
	service.store = &stateCommitFailureStore{Store: service.store, message: secret}
	service.cache = artifact.NewCache(service.store)
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}
	intent, _, err := service.records.EnsureBlobIntent(ctx, identity, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-version", int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "download-persistence-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	objectLease, err := manager.Leases().Acquire(ctx, "objects/"+objectLeaseKey(identity.Authority, identity.Repository, digest), "download-persistence-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, objectLease)
	processor := &solidifier{service: service, cfg: service.cfg, tasks: manager, cache: service.cache}
	err = processor.downloadBlobObject(ctx, &task, taskLease, contentRef{
		digest: digest, mediaType: "application/octet-stream", size: int64(len(body)),
	}, objectLease)
	var failure *tasks.Failure
	var persistenceErr *download.PersistenceError
	if !errors.As(err, &failure) || failure.Class != "storage" || !errors.As(failure.Err, &persistenceErr) {
		t.Fatalf("background download error = %v, want temporary storage failure with typed cause", err)
	}
	if !errors.Is(err, storage.ErrTimeout) || len(remote.calls) != 1 {
		t.Fatalf("background error lost its storage cause or retried upstream: error=%v calls=%d", err, len(remote.calls))
	}
}

func downloadTestLimits() config.Limits {
	return config.Limits{
		ObjectBytes: 1 << 20, DownloadMaxBytes: 64 << 20, BlobTransferTimeout: 10 * time.Second,
		RequestTimeout: time.Second, UpstreamBodyIdleTimeout: time.Second, DownloadFinalizeTimeout: time.Second,
	}
}

type stateCommitFailureStore struct {
	storage.Store
	message   string
	triggered bool
}

func (s *stateCommitFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if !s.triggered && strings.HasSuffix(key, "/state.json") {
		var value struct {
			Offset int64 `json:"offset"`
		}
		if jsonErr := json.Unmarshal(data, &value); jsonErr == nil && value.Offset > 0 {
			s.triggered = true
			return storage.Entry{}, errors.Join(storage.ErrTimeout, context.DeadlineExceeded, errors.New(s.message))
		}
	}
	return s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
}
