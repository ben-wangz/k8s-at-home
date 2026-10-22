package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"testing"
	"time"
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
