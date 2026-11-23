package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
	"registry-hub/internal/workqueue"
)

type diagnosticPendingListCounter struct {
	storage.Store
	calls atomic.Int64
}

type diagnosticDispositionFaultStore struct {
	storage.Store
	collectedWrites int
	failCollectedAt int
}

type diagnosticStorageIOCounter struct {
	storage.Store
	gets  atomic.Int64
	puts  atomic.Int64
	lists atomic.Int64
}

func (s *diagnosticStorageIOCounter) Get(ctx context.Context, key string) (storage.Object, error) {
	s.gets.Add(1)
	return s.Store.Get(ctx, key)
}

func (s *diagnosticStorageIOCounter) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.puts.Add(1)
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *diagnosticStorageIOCounter) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.lists.Add(1)
	return s.Store.List(ctx, prefix, token, limit)
}

func (s *diagnosticDispositionFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.HasPrefix(key, diagnostics.Prefix+"collected/") {
		s.collectedWrites++
		if s.failCollectedAt > 0 && s.collectedWrites == s.failCollectedAt {
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
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

func TestDiagnosticLegacyPendingPartialAcknowledgementConvergesAfterWatermark(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	now := time.Now().UTC()
	date := now.Format("20060102")
	events := []diagnostics.Event{
		{ID: diagnostics.ID("event", "legacy-partial-a"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"},
		{ID: diagnostics.ID("event", "legacy-partial-b"), OccurredAt: now.Add(time.Millisecond), Component: "background", Operation: "notify", Result: "pending"},
	}
	sources := []diagnosticPendingSource{
		{SourceKey: workqueue.Key("notify", "legacy-partial-a"), SourceVersion: "source-a", FirstEventID: events[0].ID, LastEventID: events[0].ID, EventCount: 1},
		{SourceKey: workqueue.Key("notify", "legacy-partial-b"), SourceVersion: "source-b", FirstEventID: events[1].ID, LastEventID: events[1].ID, EventCount: 1},
	}
	committed, err := (diagnostics.BatchLog{Store: f.service.store, Policy: f.service.diagnosticBatchPolicy(), Retention: f.service.diagnosticsConfig().EventRetention}).Append(ctx, events, false)
	if err != nil {
		t.Fatal(err)
	}
	firstDisposition := diagnosticEventDisposition{SourceKey: sources[0].SourceKey, SourceVersion: sources[0].SourceVersion,
		FirstEventID: sources[0].FirstEventID, LastEventID: sources[0].LastEventID, EventCount: 1,
		Disposition: "archived", BatchID: committed.ID, BatchSHA256: committed.ContentSHA256}
	if err := f.service.saveDiagnosticPendingDisposition(ctx, sources[0], firstDisposition); err != nil {
		t.Fatal(err)
	}
	partition := diagnosticPendingPartition{SchemaVersion: 1, Date: date, Batch: &diagnosticPendingBatch{
		Date: date, Critical: false, FirstAdmittedAt: now, OrdinaryDeadline: now.Add(-time.Second), Events: events, Sources: sources,
	}}
	if err := f.service.saveDiagnosticPendingPartition(ctx, date, partition); err != nil {
		t.Fatal(err)
	}
	watermarkDate := now.Add(24 * time.Hour).Format("20060102")
	if err := diagnostics.AdvanceEventHistoryWatermark(ctx, f.service.store, watermarkDate); err != nil {
		t.Fatal(err)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, false); err != nil {
		t.Fatalf("settle legacy pending partition after watermark advance: %v", err)
	}
	for index, expected := range []string{"archived", "discarded_by_policy"} {
		receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(sources[index].SourceKey)+".json")
		if err != nil || len(receipt.Dispositions) != 1 || receipt.Dispositions[0].Disposition != expected {
			t.Fatalf("source %d disposition = %+v, error = %v, want %s", index, receipt.Dispositions, err, expected)
		}
		if index == 0 && (receipt.Dispositions[0].BatchID != committed.ID || receipt.Dispositions[0].BatchSHA256 != committed.ContentSHA256) {
			t.Fatalf("existing archived disposition changed: %+v", receipt.Dispositions[0])
		}
	}
	if _, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pending partition remains after recovery: %v", err)
	}
}

func TestDiagnosticFlushPersistsOutcomeBeforeAcknowledgingSources(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	fault := &diagnosticDispositionFaultStore{Store: f.service.store, failCollectedAt: 2}
	f.service.store = fault
	ctx := context.Background()
	now := time.Now().UTC().Add(-2 * time.Second)
	events := []diagnostics.Event{
		{ID: diagnostics.ID("event", "persist-outcome-a"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"},
		{ID: diagnostics.ID("event", "persist-outcome-b"), OccurredAt: now.Add(time.Millisecond), Component: "background", Operation: "notify", Result: "pending"},
	}
	sources := []string{workqueue.Key("notify", "persist-outcome-a"), workqueue.Key("notify", "persist-outcome-b")}
	for index := range sources {
		if err := f.service.collectDiagnosticOutboxVersion(ctx, sources[index], fmt.Sprintf("source-%d", index), []diagnostics.Event{events[index]}); err != nil {
			t.Fatalf("admit source %d: %v", index, err)
		}
	}
	date := diagnostics.EventBatchDate(events[0])
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("partial flush error = %v, want injected receipt failure", err)
	}
	partition, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date))
	if err != nil || partition.Batch == nil || partition.Batch.FlushOutcome == nil || partition.Batch.FlushOutcome.Disposition != "archived" {
		t.Fatalf("persisted flush outcome = %+v, error = %v", partition.Batch, err)
	}
	first, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(sources[0])+".json")
	if err != nil || len(first.Dispositions) != 1 || first.Dispositions[0].Disposition != "archived" {
		t.Fatalf("first source receipt = %+v, error = %v", first, err)
	}
	f.service.cfg.Diagnostics.EventRetention = time.Second
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); err != nil {
		t.Fatalf("resume persisted archive outcome after retention expired: %v", err)
	}
	for index, source := range sources {
		receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(source)+".json")
		if err != nil || len(receipt.Dispositions) != 1 || receipt.Dispositions[0].Disposition != "archived" ||
			receipt.Dispositions[0].BatchID != partition.Batch.FlushOutcome.BatchID || receipt.Dispositions[0].BatchSHA256 != partition.Batch.FlushOutcome.BatchSHA256 {
			t.Fatalf("source %d receipt = %+v, error = %v", index, receipt, err)
		}
	}
	if _, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pending partition remains after outcome recovery: %v", err)
	}
}

func TestDiagnosticSourceLeaseRecordsStayBoundedAsSourcesRotate(t *testing.T) {
	counter := &diagnosticStorageIOCounter{Store: newFilesystemStore(t)}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, counter)
	ctx := context.Background()
	now := time.Now().UTC()
	for index := 0; index < 300; index++ {
		source := workqueue.Key("notify", fmt.Sprintf("rotating-source-%03d", index))
		event := diagnostics.Event{ID: diagnostics.ID("event", source), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}
		if err := f.service.collectDiagnosticOutboxVersion(ctx, source, "source-v1", []diagnostics.Event{event}); err != nil {
			t.Fatalf("collect source %d: %v", index, err)
		}
	}
	legacy, _, err := storage.Scan(ctx, f.service.store, "leases/diagnostics/event-outbox/", "", 400)
	if err != nil || len(legacy) != 0 {
		t.Fatalf("persistent per-source lease records = %d, error = %v; want none", len(legacy), err)
	}
	shards, _, err := storage.Scan(ctx, f.service.store, "leases/diagnostics/event-outbox-shards/", "", 300)
	if err != nil || len(shards) == 0 || len(shards) > 256 {
		t.Fatalf("persistent source lease shards = %d, error = %v; want 1..256", len(shards), err)
	}
	t.Logf("rotating source storage requests: LIST=%d GET=%d PUT=%d", counter.lists.Load(), counter.gets.Load(), counter.puts.Load())
	if counter.lists.Load() == 0 || counter.gets.Load() == 0 || counter.puts.Load() == 0 {
		t.Fatal("rotating source profile did not record LIST, GET, and PUT requests")
	}
	left, right := findDiagnosticSourceShardCollision()
	if diagnosticSourceLeaseShardKey(left) != diagnosticSourceLeaseShardKey(right) {
		t.Fatal("test sources do not share a source lease shard")
	}
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, diagnosticSourceLeaseShardKey(left), "test-holder", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	event := diagnostics.Event{ID: diagnostics.ID("event", right), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}
	if err := f.service.collectDiagnosticOutboxVersion(ctx, right, "source-v1", []diagnostics.Event{event}); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("collection under a held shared shard = %v, want condition failure", err)
	}
	if err := manager.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticRetentionDeletesOnlyReleasedLegacySourceLeases(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	key := diagnosticSourceLeaseKey("tasks/state/legacy-retention-test")
	holder, err := manager.Acquire(ctx, key, "legacy-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Head(ctx, "leases/"+key)
	if err != nil {
		t.Fatal(err)
	}
	var progress sourceSweepCheckpoint
	if err := f.service.cleanupReleasedDiagnosticSourceLease(ctx, entry, &progress); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, "leases/"+key); err != nil {
		t.Fatalf("active source lease was deleted: %v", err)
	}
	if err := manager.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
	entry, err = f.service.store.Head(ctx, "leases/"+key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.cleanupReleasedDiagnosticSourceLease(ctx, entry, &progress); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, "leases/"+key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("released source lease remains after cleanup: %v", err)
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

func findDiagnosticSourceShardCollision() (string, string) {
	seen := make(map[string]string)
	for index := 0; ; index++ {
		source := fmt.Sprintf("tasks/state/shard-collision-%d", index)
		shard := diagnosticSourceLeaseShardKey(source)
		if previous, ok := seen[shard]; ok {
			return previous, source
		}
		seen[shard] = source
	}
}
