package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
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
		cfg:   config.Config{Limits: config.Limits{ManifestBytes: 1 << 20, MaxDescriptors: 10, DescriptorDataBytes: 1 << 20}},
		store: store, registry: registry, tasks: manager, gate: gate, metrics: newMetrics(),
	}
	processor := &solidifier{service: service, cfg: service.cfg, tasks: manager}

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
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)

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
	if _, err := manager.Cancel(ctx, task.ID); !errors.Is(err, tasks.ErrRootPublishing) {
		t.Fatalf("Cancel() during fallback index publication error = %v, want conflict", err)
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
	if _, err := manager.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("Cancel() after fallback index publication error = %v", err)
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
