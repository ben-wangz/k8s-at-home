package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

type solidifierUploadTestOptions struct {
	putStatuses            []int
	partialBytes           int
	commitDigest           string
	omitCommitDigest       bool
	dropCommitResponse     bool
	verifyStatus           int
	cancelAfterCreate      bool
	cancelBeforeCheckpoint bool
	cleanupStatus          int
	blockCleanup           bool
	blockPUT               bool
	invalidLocation        string

	logOutput *bytes.Buffer
}

type solidifierUploadTestRegistry struct {
	t       *testing.T
	options solidifierUploadTestOptions

	mu               sync.Mutex
	manager          *tasks.Manager
	taskID           string
	data             []byte
	digest           string
	uploadPath       string
	active           bool
	currentToken     string
	partialBytes     int
	committedData    []byte
	headRequests     int
	postRequests     int
	putRequests      int
	deleteRequests   int
	statusRequests   int
	lastCleanupToken string
	setupErr         error
	cleanupStarted   chan struct{}
	putStarted       chan struct{}
	finishPUT        chan struct{}
	finishCleanup    chan struct{}
}

type solidifierBlobProcessor struct {
	solidifier *solidifier
	blob       contentRef
}

func (p *solidifierBlobProcessor) Process(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	return p.solidifier.ensureBlob(ctx, task, taskLease, p.blob)
}

type solidifierUploadTestFixture struct {
	registry   *solidifierUploadTestRegistry
	solidifier *solidifier
	manager    *tasks.Manager
	task       tasks.Task
	data       []byte
	digest     string
}

func newSolidifierUploadTestFixture(t *testing.T, options solidifierUploadTestOptions) *solidifierUploadTestFixture {
	t.Helper()
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := artifact.NewCache(store)
	data := []byte("solidifier upload cleanup payload")
	digest := artifact.CalculateSHA256(data)
	const mediaType = "application/vnd.oci.image.layer.v1.tar"
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, mediaType, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("cache solidification blob: %v", err)
	}
	registryState := &solidifierUploadTestRegistry{
		t: t, options: options, data: data, digest: digest,
		cleanupStarted: make(chan struct{}), putStarted: make(chan struct{}), finishPUT: make(chan struct{}), finishCleanup: make(chan struct{}),
	}
	registry := newLocalRegistry(t, http.HandlerFunc(registryState.serveHTTP))
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatalf("create write gate: %v", err)
	}
	var loggerOutput io.Writer = io.Discard
	if options.logOutput != nil {
		loggerOutput = options.logOutput
	}
	service := &Service{
		store: store, cache: cache, registry: registry, gate: gate, metrics: newMetrics(), records: tasks.NewRecords(store),
		logger: slog.New(slog.NewJSONHandler(loggerOutput, nil)),
	}
	solidification := &solidifier{
		service: service, cfg: config.Config{Limits: config.Limits{BlobTransferTimeout: 5 * time.Second}},
		cache: cache, store: store, registry: registry, gate: gate,
	}
	manager, err := tasks.NewManager(store, 1, 10, &solidifierBlobProcessor{
		solidifier: solidification,
		blob:       contentRef{digest: digest, mediaType: mediaType, size: int64(len(data))},
	})
	if err != nil {
		t.Fatalf("create task manager: %v", err)
	}
	t.Cleanup(manager.Close)
	service.tasks = manager
	solidification.tasks = manager
	registryState.manager = manager
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}
	intent, _, err := service.records.EnsureBlobIntent(ctx, identity, 0, "")
	if err != nil {
		t.Fatalf("create blob intent: %v", err)
	}
	cached, err := cache.GetAnyRetained(ctx, identity.Authority, identity.Repository, digest)
	if err != nil {
		t.Fatalf("read cached blob version: %v", err)
	}
	cacheVersion := cached.DataVersion
	_ = cached.Body.Close()
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, mediaType, cacheVersion, int64(len(data)))
	if err != nil {
		t.Fatalf("set blob intent ready: %v", err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatalf("enqueue blob task: %v", err)
	}
	registryState.taskID = task.ID
	return &solidifierUploadTestFixture{
		registry: registryState, solidifier: solidification, manager: manager,
		task: task, data: data, digest: digest,
	}
}

func (f *solidifierUploadTestFixture) acquireTaskLease(t *testing.T) lease.Lease {
	t.Helper()
	taskLease, err := f.manager.Leases().Acquire(context.Background(), "tasks/"+f.task.ID, "upload-cleanup-test", time.Minute)
	if err != nil {
		t.Fatalf("acquire task lease: %v", err)
	}
	t.Cleanup(func() { _ = f.manager.Leases().Release(context.Background(), taskLease) })
	return taskLease
}

func (r *solidifierUploadTestRegistry) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if strings.HasSuffix(request.URL.Path, "/blobs/uploads/") && request.Method == http.MethodPost {
		r.createUpload(writer, request)
		return
	}
	if strings.Contains(request.URL.Path, "/blobs/uploads/") {
		r.handleUpload(writer, request)
		return
	}
	if strings.HasSuffix(request.URL.Path, "/blobs/"+r.digest) {
		r.handleBlob(writer, request)
		return
	}
	http.NotFound(writer, request)
}

func (r *solidifierUploadTestRegistry) createUpload(writer http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.postRequests++
	r.uploadPath = strings.TrimSuffix(request.URL.Path, "/") + "/session"
	r.active = true
	r.currentToken = "offset=0"
	r.partialBytes = 0
	location := r.options.invalidLocation
	if location == "" {
		location = r.location(r.currentToken)
	}
	if r.options.cancelAfterCreate {
		_, r.setupErr = r.manager.Cancel(context.Background(), r.taskID)
	}
	writer.Header().Set("Location", location)
	writer.WriteHeader(http.StatusAccepted)
}

func (r *solidifierUploadTestRegistry) handleUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPut && r.options.blockPUT {
		r.mu.Lock()
		r.putRequests++
		close(r.putStarted)
		r.mu.Unlock()
		select {
		case <-request.Context().Done():
		case <-r.finishPUT:
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch request.Method {
	case http.MethodPut:
		r.putRequests++
		if index := r.putRequests - 1; index < len(r.options.putStatuses) {
			if r.options.partialBytes > 0 {
				r.partialBytes = r.options.partialBytes
				r.currentToken = fmt.Sprintf("offset=%d", r.partialBytes)
			}
			writer.WriteHeader(r.options.putStatuses[index])
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read blob failed", http.StatusBadRequest)
			return
		}
		r.committedData = body
		r.active = false
		returnedDigest := r.digest
		if r.options.commitDigest != "" {
			returnedDigest = r.options.commitDigest
		}
		if r.options.dropCommitResponse {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				r.t.Error("test server does not support hijacking the commit response")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				r.t.Errorf("hijack commit response: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		if !r.options.omitCommitDigest {
			writer.Header().Set("Docker-Content-Digest", returnedDigest)
		}
		writer.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		r.statusRequests++
		if !r.active {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Location", r.location(r.currentToken))
		writer.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		r.deleteRequests++
		r.lastCleanupToken = request.URL.Query().Get("_state")
		if r.options.blockCleanup {
			close(r.cleanupStarted)
			r.mu.Unlock()
			<-r.finishCleanup
			r.mu.Lock()
		}
		if r.options.cleanupStatus != 0 {
			writer.WriteHeader(r.options.cleanupStatus)
			return
		}
		if !r.active {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.URL.Query().Get("_state") != r.currentToken {
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		r.active = false
		r.partialBytes = 0
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.Error(writer, "unsupported upload method", http.StatusMethodNotAllowed)
	}
}

func (r *solidifierUploadTestRegistry) handleBlob(writer http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if request.Method == http.MethodHead {
		r.headRequests++
		if len(r.committedData) == 0 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(r.committedData)))
		writer.Header().Set("Docker-Content-Digest", r.digest)
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodGet {
		if r.options.verifyStatus != 0 {
			writer.WriteHeader(r.options.verifyStatus)
			return
		}
		if len(r.committedData) == 0 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if r.options.cancelBeforeCheckpoint {
			_, r.setupErr = r.manager.Cancel(context.Background(), r.taskID)
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(r.committedData)))
		writer.Header().Set("Docker-Content-Digest", r.digest)
		_, _ = writer.Write(r.committedData)
		return
	}
	http.NotFound(writer, request)
}

func (r *solidifierUploadTestRegistry) location(token string) string {
	query := url.Values{"_state": []string{token}}
	return r.uploadPath + "?" + query.Encode()
}

func (f *solidifierUploadTestFixture) runBlob(ctx context.Context, taskLease lease.Lease) error {
	return f.solidifier.ensureBlob(ctx, &f.task, taskLease, contentRef{
		digest: f.digest, mediaType: "application/vnd.oci.image.layer.v1.tar", size: int64(len(f.data)),
	})
}

func TestSolidifierEnsureBlobCancelsPartialUploadBeforeReturningFailure(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, partialBytes: 5,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() error = %#v, want temporary distribution failure", err)
	}
	if fixture.registry.active || fixture.registry.partialBytes != 0 || fixture.registry.deleteRequests != 2 || fixture.registry.statusRequests != 1 {
		t.Fatalf("upload active=%t partial=%d DELETEs=%d status GETs=%d", fixture.registry.active, fixture.registry.partialBytes, fixture.registry.deleteRequests, fixture.registry.statusRequests)
	}
}

func TestSolidifierEnsureBlobCancelsAfterTaskCancellationBeforePut(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{cancelAfterCreate: true})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	if !errors.Is(err, tasks.ErrCancelled) {
		t.Fatalf("ensureBlob() error = %v, want task cancellation", err)
	}
	if fixture.registry.setupErr != nil {
		t.Fatalf("cancel task during upload creation: %v", fixture.registry.setupErr)
	}
	if fixture.registry.putRequests != 0 || fixture.registry.deleteRequests != 1 || fixture.registry.active {
		t.Fatalf("PUTs=%d DELETEs=%d active=%t, want no PUT and a cancelled session", fixture.registry.putRequests, fixture.registry.deleteRequests, fixture.registry.active)
	}
}

func TestSolidifierEnsureBlobKeepsConfirmedCommitAfterVerificationFailure(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{verifyStatus: http.StatusServiceUnavailable})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() error = %#v, want verification failure", err)
	}
	if fixture.registry.deleteRequests != 0 || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("confirmed blob was cancelled or lost: DELETEs=%d committed=%q", fixture.registry.deleteRequests, fixture.registry.committedData)
	}
}

func TestSolidifierEnsureBlobDoesNotCancelCommittedBlobAfterCheckpointCancellation(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{cancelBeforeCheckpoint: true})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	if !errors.Is(err, tasks.ErrCancelled) {
		t.Fatalf("ensureBlob() error = %v, want cancellation during checkpoint", err)
	}
	if fixture.registry.setupErr != nil {
		t.Fatalf("cancel task after commit: %v", fixture.registry.setupErr)
	}
	if fixture.registry.deleteRequests != 0 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("confirmed blob was cancelled or lost: DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
	}
}

func TestSolidifierEnsureBlobHandlesCommitDigestFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		options solidifierUploadTestOptions
	}{
		{name: "missing digest", options: solidifierUploadTestOptions{omitCommitDigest: true}},
		{name: "wrong digest", options: solidifierUploadTestOptions{commitDigest: "sha256:wrong"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSolidifierUploadTestFixture(t, test.options)
			err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
			var failure *tasks.Failure
			if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
				t.Fatalf("ensureBlob() error = %#v, want temporary distribution failure", err)
			}
			if fixture.registry.deleteRequests != 1 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
				t.Fatalf("DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
			}
		})
	}
}

func TestSolidifierEnsureBlobFindsBlobAfterCommitResponseLoss(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{dropCommitResponse: true})
	taskLease := fixture.acquireTaskLease(t)
	err := fixture.runBlob(context.Background(), taskLease)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() first attempt error = %#v, want temporary distribution failure", err)
	}
	if fixture.registry.deleteRequests != 1 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("lost commit response left DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
	}
	if err := fixture.runBlob(context.Background(), taskLease); err != nil {
		t.Fatalf("ensureBlob() retry for committed blob: %v", err)
	}
	if fixture.registry.postRequests != 1 || fixture.registry.putRequests != 1 || fixture.registry.headRequests != 2 {
		t.Fatalf("POSTs=%d PUTs=%d HEADs=%d, want one upload and an existing-blob retry", fixture.registry.postRequests, fixture.registry.putRequests, fixture.registry.headRequests)
	}
}

func TestSolidifierEnsureBlobPreservesPrimaryErrorWhenCancellationFails(t *testing.T) {
	var output bytes.Buffer
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, cleanupStatus: http.StatusInternalServerError, logOutput: &output,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "distribution" || failure.Permanent {
		t.Fatalf("ensureBlob() error = %#v, want original temporary distribution failure", err)
	}
	if fixture.registry.deleteRequests != 1 || !strings.Contains(output.String(), "internal upload cleanup was not confirmed") {
		t.Fatalf("cleanup DELETEs=%d log=%q", fixture.registry.deleteRequests, output.String())
	}
	if strings.Contains(output.String(), "_state") || strings.Contains(output.String(), "offset%3D0") {
		t.Fatalf("cleanup log exposed upload state: %s", output.String())
	}
}

func TestSolidifierEnsureBlobRefusesMalformedLocationWithoutOutboundCleanup(t *testing.T) {
	var output bytes.Buffer
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		invalidLocation: "https://outside.example/v2/other/blobs/uploads/session?_state=sensitive",
		logOutput:       &output,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "distribution_invalid" {
		t.Fatalf("ensureBlob() error = %#v, want permanent invalid location", err)
	}
	if fixture.registry.deleteRequests != 0 || fixture.registry.statusRequests != 0 || !strings.Contains(output.String(), "internal upload cleanup was not confirmed") {
		t.Fatalf("DELETEs=%d status GETs=%d log=%q", fixture.registry.deleteRequests, fixture.registry.statusRequests, output.String())
	}
	if strings.Contains(output.String(), "sensitive") || strings.Contains(output.String(), "outside.example") {
		t.Fatalf("cleanup log exposed the invalid location: %s", output.String())
	}
}

func TestSolidifierEnsureBlobHonorsCancelledWorkContextDuringPut(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{blockPUT: true})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	taskLease := fixture.acquireTaskLease(t)
	go func() { result <- fixture.runBlob(ctx, taskLease) }()
	select {
	case <-fixture.registry.putStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("PUT did not start")
	}
	cancel()
	close(fixture.registry.finishPUT)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("ensureBlob() succeeded after its work context was cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ensureBlob() did not finish after cancellation")
	}
	if fixture.registry.deleteRequests != 1 || fixture.registry.active {
		t.Fatalf("cleanup DELETEs=%d active=%t", fixture.registry.deleteRequests, fixture.registry.active)
	}
}

func TestSolidifierUploadRetryDoesNotAccumulateDistributionSessions(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, partialBytes: 5,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.manager.Start(ctx)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		current, err := fixture.manager.Get(ctx, fixture.task.ID)
		if err != nil {
			t.Fatalf("read task state: %v", err)
		}
		if current.Status == tasks.Solidified {
			if current.RetryCount != 1 || current.Checkpoints[fixture.digest] != "verified" {
				t.Fatalf("completed task retry/checkpoint state = %d/%q", current.RetryCount, current.Checkpoints[fixture.digest])
			}
			if fixture.registry.postRequests != 2 || fixture.registry.deleteRequests != 2 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
				t.Fatalf("POSTs=%d DELETEs=%d active=%t committed=%q", fixture.registry.postRequests, fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
			}
			return
		}
		if current.Status == tasks.RetryWait && current.RetryCount != 1 {
			t.Fatalf("retry count = %d, want exactly 1", current.RetryCount)
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := fixture.manager.Get(ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("task did not solidify after retry: state=%q retryCount=%d errorClass=%q", current.Status, current.RetryCount, current.ErrorClass)
}

func TestSolidifierRepeatedFailuresCancelEachUploadSession(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable}, partialBytes: 5,
	})
	taskLease := fixture.acquireTaskLease(t)
	for attempt := 1; attempt <= 3; attempt++ {
		err := fixture.runBlob(context.Background(), taskLease)
		var failure *tasks.Failure
		if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
			t.Fatalf("attempt %d error = %#v, want temporary distribution failure", attempt, err)
		}
		if fixture.registry.active || fixture.registry.partialBytes != 0 || fixture.registry.postRequests != attempt || fixture.registry.deleteRequests != 2*attempt {
			t.Fatalf("after attempt %d: active=%t partial=%d POSTs=%d DELETEs=%d", attempt, fixture.registry.active, fixture.registry.partialBytes, fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	}
	if err := fixture.runBlob(context.Background(), taskLease); err != nil {
		t.Fatalf("ensureBlob() successful attempt: %v", err)
	}
	if fixture.registry.active || fixture.registry.postRequests != 4 || fixture.registry.deleteRequests != 6 || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("final state: active=%t POSTs=%d DELETEs=%d committed=%q", fixture.registry.active, fixture.registry.postRequests, fixture.registry.deleteRequests, fixture.registry.committedData)
	}
}

func TestSolidifierEnsureBlobDoesNotCreateUploadForExistingBlobOrBusyLease(t *testing.T) {
	t.Run("existing blob", func(t *testing.T) {
		fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{})
		fixture.registry.committedData = append([]byte(nil), fixture.data...)
		err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
		if err != nil {
			t.Fatalf("ensureBlob() existing blob error = %v", err)
		}
		if fixture.registry.postRequests != 0 || fixture.registry.deleteRequests != 0 {
			t.Fatalf("existing blob caused POSTs=%d DELETEs=%d", fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	})

	t.Run("busy content lease", func(t *testing.T) {
		fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{})
		contentLease, err := fixture.manager.Leases().Acquire(context.Background(), "objects/"+objectLeaseKey("ghcr.io", "team/image", fixture.digest), "other-worker", time.Minute)
		if err != nil {
			t.Fatalf("acquire competing content lease: %v", err)
		}
		t.Cleanup(func() { _ = fixture.manager.Leases().Release(context.Background(), contentLease) })
		err = fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
		var failure *tasks.Failure
		if !errors.As(err, &failure) || failure.Class != "object_busy" {
			t.Fatalf("ensureBlob() error = %#v, want object_busy", err)
		}
		if fixture.registry.headRequests != 0 || fixture.registry.postRequests != 0 || fixture.registry.deleteRequests != 0 {
			t.Fatalf("busy lease caused HEAD=%d POST=%d DELETE=%d", fixture.registry.headRequests, fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	})
}

func TestSolidifierUploadCleanupCompletesBeforeWriteGateQuiesces(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, blockCleanup: true,
	})
	taskLease := fixture.acquireTaskLease(t)
	result := make(chan error, 1)
	go func() { result <- fixture.runBlob(context.Background(), taskLease) }()
	select {
	case <-fixture.registry.cleanupStarted:
	case <-time.After(2 * time.Second):
		close(fixture.registry.finishCleanup)
		t.Fatal("upload cancellation did not start")
	}
	ctx := context.Background()
	if err := fixture.solidifier.gate.BeginDrain(ctx); err != nil {
		close(fixture.registry.finishCleanup)
		t.Fatalf("begin gate drain: %v", err)
	}
	fixture.solidifier.gate.mu.Lock()
	activeWrites := fixture.solidifier.gate.active
	fixture.solidifier.gate.mu.Unlock()
	if activeWrites != 1 {
		close(fixture.registry.finishCleanup)
		t.Fatalf("active writes during blocked cleanup = %d, want 1", activeWrites)
	}
	close(fixture.registry.finishCleanup)
	if err := <-result; err == nil {
		t.Fatal("solidification unexpectedly succeeded")
	}
	if err := fixture.solidifier.gate.WaitQuiesced(ctx, time.Second); err != nil {
		t.Fatalf("gate did not quiesce after cleanup: %v", err)
	}
	if fixture.registry.active {
		t.Fatal("gate quiesced with the upload session still active")
	}
}
