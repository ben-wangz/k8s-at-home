package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
	"registry-hub/internal/workqueue"
)

type diagnosticPendingListCounter struct {
	storage.Store
	calls atomic.Int64
}

func (s *diagnosticPendingListCounter) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if prefix == diagnosticPendingBatchPrefix {
		s.calls.Add(1)
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func TestDiagnosticPendingWorkerUsesBatchDeadlineInsteadOfPolling(t *testing.T) {
	base := newFilesystemStore(t)
	counter := &diagnosticPendingListCounter{Store: base}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counter)
	f.service.diagnosticBatchWake = make(chan struct{}, 1)
	f.service.diagnosticSourceWakes = make(chan diagnosticSourceWake, 2)
	f.service.diagnosticSourcePending = make(map[string]struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.runDiagnosticSourceWakeSink(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("diagnostic worker did not stop")
		}
	})

	waitForDiagnosticListCount(t, counter, 1)
	time.Sleep(400 * time.Millisecond)
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("idle pending-prefix LIST calls = %d, want one startup recovery scan", got)
	}

	now := time.Now().UTC()
	event := diagnostics.Event{ID: diagnostics.ID("event", "pending-deadline"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}
	source := workqueue.Key("notify", "pending-deadline")
	date := diagnostics.EventBatchDate(event)
	partition := diagnosticPendingPartition{SchemaVersion: 1, Date: date, Batch: &diagnosticPendingBatch{
		Date: date, FirstAdmittedAt: now, OrdinaryDeadline: now.Add(250 * time.Millisecond),
		Events: []diagnostics.Event{event}, Sources: []diagnosticPendingSource{{
			SourceKey: source, SourceVersion: "source-v1", FirstEventID: event.ID, LastEventID: event.ID,
			EventCount: 1, ContentSHA256: "test-range",
		}},
	}}
	if err := f.service.saveDiagnosticPendingPartition(context.Background(), date, partition); err != nil {
		t.Fatal(err)
	}
	f.service.signalDiagnosticBatchWake()
	waitForDiagnosticListCount(t, counter, 2)
	waitForDiagnosticPendingRemoval(t, f.service, date)
	if got := counter.calls.Load(); got > 3 {
		t.Fatalf("deadline-driven pending-prefix LIST calls = %d, want startup, admission, and at most one deadline scan", got)
	}
}

func TestDiagnosticPendingWorkerIdleS3IssuesOnlyStartupList(t *testing.T) {
	var lists atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Query().Get("list-type") != "2" {
			http.NotFound(writer, request)
			return
		}
		lists.Add(1)
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(writer, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>registry-hub</Name><Prefix>diagnostics/v2/event-batch-pending/</Prefix><KeyCount>0</KeyCount><MaxKeys>100</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()
	store, err := s3store.New(context.Background(), config.S3{
		AccessKeyID: "test-key", SecretAccessKey: "test-secret", Endpoint: server.URL, Region: "us-east-1",
		Bucket: "registry-hub", UsePathStyle: true, AllowHTTP: true, Timeout: 3 * time.Second,
		MaxConnections: 2, ReplayDirectory: t.TempDir(), MultipartThreshold: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store, metrics: newMetrics(), diagnosticBatchWake: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runDiagnosticSourceWakeSink(ctx)
	}()
	waitForAtomicCount(t, &lists, 1)
	time.Sleep(600 * time.Millisecond)
	if got := lists.Load(); got != 1 {
		t.Fatalf("idle S3 ListObjectsV2 calls = %d, want one startup recovery scan", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic worker did not stop")
	}
}

func newFilesystemStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func waitForDiagnosticListCount(t *testing.T, counter *diagnosticPendingListCounter, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counter.calls.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending-prefix LIST calls = %d, want at least %d", counter.calls.Load(), want)
}

func waitForAtomicCount(t *testing.T, counter *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("S3 ListObjectsV2 calls = %d, want at least %d", counter.Load(), want)
}

func waitForDiagnosticPendingRemoval(t *testing.T, service *Service, date string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, err := inventory.Read[diagnosticPendingPartition](context.Background(), service.store, diagnosticPendingBatchKey(date))
		if errors.Is(err, storage.ErrNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending batch did not flush at its deadline")
}
