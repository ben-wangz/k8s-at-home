package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"testing"
	"time"
)

type fallbackPublicationFixture struct {
	ctx                 context.Context
	store               *filesystem.Store
	manager             *tasks.Manager
	service             *Service
	processor           *solidifier
	task                tasks.Task
	taskLease           lease.Lease
	subjectDigest       string
	concurrentDigest    string
	manifestData        []byte
	manifestDigest      string
	fallbackReadStarted chan struct{}
	releaseFallbackRead chan struct{}
	startOnce           sync.Once
	releaseOnce         sync.Once
	objectMu            sync.Mutex
	fallbackIndex       []byte
	fallbackGets        int
	fallbackPuts        int
	leaseProbeFailures  []string
	result              chan error
}

func TestCancelIsFencedAndFallbackPublicationRetriesConcurrentIndexConflict(t *testing.T) {
	fixture := newFallbackPublicationFixture(t)
	fixture.beginPublication(t)
	fixture.assertCancellationKeepsProtection(t)
	fixture.completePublication(t)
	assertConcurrentFallbackIndex(t, fixture)
}

func newFallbackPublicationFixture(t *testing.T) *fallbackPublicationFixture {
	t.Helper()
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	fixture := &fallbackPublicationFixture{
		ctx: ctx, store: store, manager: manager,
		subjectDigest:       artifact.CalculateSHA256([]byte("subject")),
		concurrentDigest:    artifact.CalculateSHA256([]byte("concurrent fallback referrer")),
		fallbackReadStarted: make(chan struct{}), releaseFallbackRead: make(chan struct{}),
	}
	server := newFallbackPublicationServer(t, fixture)
	t.Cleanup(func() {
		fixture.releaseOnce.Do(func() { close(fixture.releaseFallbackRead) })
		server.Close()
	})
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = &Service{
		ctx: ctx,
		cfg: config.Config{
			GCMode: "local",
			AdminAuth: config.Auth{Basic: []config.Credential{{
				Username: "operator", Password: "secret", Permissions: []string{auth.TaskAdmin, auth.GCAdmin},
			}}},
			Limits: config.Limits{ManifestBytes: 1 << 20, MaxDescriptors: 10, DescriptorDataBytes: 1 << 20},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:  store, registry: registry, tasks: manager, records: tasks.NewRecords(store), gate: gate, metrics: newMetrics(),
	}
	fixture.processor = &solidifier{service: fixture.service, cfg: fixture.service.cfg, tasks: manager, gate: gate}
	fixture.service.gc = &gcController{service: fixture.service}
	fixture.prepareTask(t)
	return fixture
}

func newFallbackPublicationServer(t *testing.T, fixture *fallbackPublicationFixture) *httptest.Server {
	t.Helper()
	concurrentDescriptor, err := json.Marshal(map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest":    fixture.concurrentDigest,
		"size":      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	concurrentIndex, err := json.Marshal(referrerDocument{
		SchemaVersion: 2, MediaType: referrerIndexMediaType,
		Manifests: []json.RawMessage{concurrentDescriptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.WriteHeader(http.StatusOK)
		case strings.Contains(request.URL.Path, "/manifests/") && request.Method == http.MethodGet:
			fixture.serveFallbackIndexRead(writer, request)
		case strings.Contains(request.URL.Path, "/manifests/") && request.Method == http.MethodPut:
			fixture.serveFallbackIndexWrite(writer, request, concurrentIndex)
		default:
			http.NotFound(writer, request)
		}
	}))
}

func (fixture *fallbackPublicationFixture) prepareTask(t *testing.T) {
	t.Helper()
	configData := []byte("{}")
	configDigest := artifact.CalculateSHA256(configData)
	manifestData, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config:  &artifact.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: configDigest, Size: int64(len(configData))},
		Layers:  []artifact.Descriptor{},
		Subject: &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: fixture.subjectDigest, Size: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifestData = manifestData
	fixture.manifestDigest = artifact.CalculateSHA256(fixture.manifestData)
	task, _, err := fixture.manager.Enqueue(fixture.ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := artifact.CalculateSHA256([]byte("root"))
	if err := fixture.manager.SetDiscovered(fixture.ctx, &task, rootDigest, ociManifestMediaType); err != nil {
		t.Fatal(err)
	}
	scopeID, err := protectionScopeID(task.Authority, task.Repository, rootDigest, task.Generation)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("c", 32)
	if err := fixture.service.gate.AdmitProtection(fixture.ctx, sessionID, scopeID, task.ID, 100, 10, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.SetProtectionState(fixture.ctx, task.ID, scopeID, sessionID, 100, true); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.manager.Get(fixture.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.taskLease, err = fixture.manager.Leases().Acquire(fixture.ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.manager.Leases().Release(fixture.ctx, fixture.taskLease) })
	fixture.task.Status = tasks.Running
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.task, fixture.taskLease); err != nil {
		t.Fatal(err)
	}
	fixture.task.ProtectionSessionID = sessionID
}
