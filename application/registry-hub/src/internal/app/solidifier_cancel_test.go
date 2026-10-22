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

func TestCancelIsFencedAndFallbackPublicationRetriesConcurrentIndexConflict(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	subjectDigest := artifact.CalculateSHA256([]byte("subject"))
	fallbackTag := referrerFallbackTag(subjectDigest)
	concurrentDigest := artifact.CalculateSHA256([]byte("concurrent fallback referrer"))
	concurrentDescriptor, err := json.Marshal(map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest":    concurrentDigest,
		"size":      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	concurrentIndex, err := json.Marshal(referrerDocument{
		SchemaVersion: 2,
		MediaType:     referrerIndexMediaType,
		Manifests:     []json.RawMessage{concurrentDescriptor},
	})
	if err != nil {
		t.Fatal(err)
	}

	fallbackReadStarted := make(chan struct{})
	releaseFallbackRead := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	var objectMu sync.Mutex
	var fallbackIndex []byte
	var fallbackGets, fallbackPuts int
	var leaseProbeFailures []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.WriteHeader(http.StatusOK)
		case strings.Contains(request.URL.Path, "/manifests/") && request.Method == http.MethodGet:
			objectMu.Lock()
			fallbackGets++
			data := append([]byte(nil), fallbackIndex...)
			objectMu.Unlock()
			if len(data) == 0 {
				startOnce.Do(func() { close(fallbackReadStarted) })
				select {
				case <-releaseFallbackRead:
				case <-request.Context().Done():
					return
				}
				writeManifestMiss(writer)
				return
			}
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(data))
			_, _ = writer.Write(data)
		case strings.Contains(request.URL.Path, "/manifests/") && request.Method == http.MethodPut:
			data, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "invalid body", http.StatusBadRequest)
				return
			}
			indexDigest := artifact.CalculateSHA256(data)
			for _, key := range []string{
				"referrers/" + objectLeaseKey("ghcr.io", "team/image", subjectDigest),
				tagLeaseKey("ghcr.io", "team/image", fallbackTag),
				"objects/" + objectLeaseKey("ghcr.io", "team/image", indexDigest),
			} {
				probe, probeErr := manager.Leases().Acquire(request.Context(), key, "acceptance-probe", time.Minute)
				if probeErr == nil {
					_ = manager.Leases().Release(request.Context(), probe)
					objectMu.Lock()
					leaseProbeFailures = append(leaseProbeFailures, key+" was not held during index write")
					objectMu.Unlock()
				} else if !errors.Is(probeErr, lease.ErrHeld) {
					objectMu.Lock()
					leaseProbeFailures = append(leaseProbeFailures, key+": "+probeErr.Error())
					objectMu.Unlock()
				}
			}
			objectMu.Lock()
			fallbackPuts++
			putNumber := fallbackPuts
			if putNumber == 1 {
				fallbackIndex = append([]byte(nil), concurrentIndex...)
			} else {
				fallbackIndex = append([]byte(nil), data...)
			}
			objectMu.Unlock()
			if putNumber == 1 {
				writer.WriteHeader(http.StatusConflict)
				return
			}
			writer.Header().Set("Docker-Content-Digest", indexDigest)
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(releaseFallbackRead) })

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
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
	processor := &solidifier{service: service, cfg: service.cfg, tasks: manager, gate: gate}
	service.gc = &gcController{service: service}

	configData := []byte("{}")
	configDigest := artifact.CalculateSHA256(configData)
	manifestData, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        &artifact.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: configDigest, Size: int64(len(configData))},
		Layers:        []artifact.Descriptor{},
		Subject:       &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: subjectDigest, Size: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := artifact.CalculateSHA256(manifestData)
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := artifact.CalculateSHA256([]byte("root"))
	if err := manager.SetDiscovered(ctx, &task, rootDigest, ociManifestMediaType); err != nil {
		t.Fatal(err)
	}
	scopeID, err := protectionScopeID(task.Authority, task.Repository, rootDigest, task.Generation)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("c", 32)
	if err := gate.AdmitProtection(ctx, sessionID, scopeID, task.ID, 100, 10, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetProtectionState(ctx, task.ID, scopeID, sessionID, 100, true); err != nil {
		t.Fatal(err)
	}
	task, err = manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	task.Status = tasks.Running
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	// The per-manifest session is temporary task context and is not persisted on root tasks.
	task.ProtectionSessionID = sessionID

	result := make(chan error, 1)
	go func() {
		result <- processor.syncReferrerManifest(ctx, &task, taskLease, manifestData, ociManifestMediaType, manifestDigest, "", false)
	}()
	select {
	case <-fallbackReadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback index publication did not reach its read-modify-write phase")
	}

	persisted, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.RootPublishing || persisted.RootPublished {
		t.Fatalf("fallback publication fence state = %#v", persisted)
	}
	cancelRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", nil)
	cancelRequest.SetBasicAuth("operator", "secret")
	cancelResponse := httptest.NewRecorder()
	service.handleAdmin(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusAccepted {
		t.Fatalf("cancel during fallback index publication returned %d: %s", cancelResponse.Code, cancelResponse.Body.String())
	}
	var cancelView adminTaskView
	if err := json.Unmarshal(cancelResponse.Body.Bytes(), &cancelView); err != nil {
		t.Fatal(err)
	}
	if cancelView.ID != task.ID || cancelView.Status != tasks.Running || !cancelView.CancelRequested || !cancelView.GCProtectionActive {
		t.Fatalf("cancel response did not retain the running protection state: %#v", cancelView)
	}
	gcRequest := httptest.NewRequest(http.MethodPost, "/api/v1/gc", nil)
	gcRequest.SetBasicAuth("operator", "secret")
	gcResponse := httptest.NewRecorder()
	service.handleAdmin(gcResponse, gcRequest)
	if gcResponse.Code != http.StatusConflict || !strings.Contains(gcResponse.Body.String(), `"code":"GC_BLOCKED"`) {
		t.Fatalf("GC after accepted non-quiescent cancellation returned %d: %s", gcResponse.Code, gcResponse.Body.String())
	}
	if gate.State().State != "open" || gate.State().ProtectionSessionCount != 1 {
		t.Fatalf("accepted cancellation changed the GC gate before quiescence: %#v", gate.State())
	}

	releaseOnce.Do(func() { close(releaseFallbackRead) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("syncReferrerManifest() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fallback index publication did not finish")
	}
	persisted, err = manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RootPublishing || persisted.RootPublished {
		t.Fatalf("completed non-root publication retained its fence: %#v", persisted)
	}
	persisted.Status = tasks.Cancelled
	if err := manager.SaveProgress(ctx, &persisted, taskLease); err != nil {
		t.Fatalf("save task quiescence after publication = %v", err)
	}
	if err := processor.TaskTerminal(ctx, &persisted); err != nil {
		t.Fatalf("release protection after task quiescence = %v", err)
	}
	if !persisted.TerminalCleanupComplete || persisted.GCProtected || gate.State().ProtectionSessionCount != 0 {
		t.Fatalf("quiescent task retained GC protection: task=%#v gate=%#v", persisted, gate.State())
	}
	assertConcurrentFallbackIndex(t, &objectMu, fallbackIndex, fallbackGets, fallbackPuts, leaseProbeFailures, manifestDigest, concurrentDigest)
}

func assertConcurrentFallbackIndex(t *testing.T, objectMu *sync.Mutex, fallbackIndex []byte, fallbackGets, fallbackPuts int, leaseProbeFailures []string, manifestDigest, concurrentDigest string) {
	t.Helper()
	objectMu.Lock()
	defer objectMu.Unlock()
	if len(fallbackIndex) == 0 || !bytes.Contains(fallbackIndex, []byte(manifestDigest)) || !bytes.Contains(fallbackIndex, []byte(concurrentDigest)) {
		t.Fatalf("fallback index did not preserve both referrers after conflict retry: %s", fallbackIndex)
	}
	if fallbackGets != 3 || fallbackPuts != 2 {
		t.Fatalf("fallback index operations = GET %d PUT %d, want read/conflict/re-read/write/read", fallbackGets, fallbackPuts)
	}
	if len(leaseProbeFailures) != 0 {
		t.Fatalf("fallback publication lease checks failed: %v", leaseProbeFailures)
	}
}
