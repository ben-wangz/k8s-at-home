package app

import (
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
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReferrerRepairReplaysPartialFallbackWrite(t *testing.T) {
	ctx := context.Background()
	subject := artifact.CalculateSHA256([]byte("subject"))
	subjectBody := []byte("subject")
	referrerBody, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Subject:       &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: subject, Size: int64(len(subjectBody))},
	})
	if err != nil {
		t.Fatal(err)
	}
	referrer := artifact.CalculateSHA256(referrerBody)
	var mu sync.Mutex
	var fallbackIndex []byte
	fallbackPutAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"name":"ghcr.io/team/image","tags":[]}`)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrer):
			writer.Header().Set("Content-Type", ociManifestMediaType)
			writer.Header().Set("Docker-Content-Digest", referrer)
			writer.Header().Set("Content-Length", strconv.Itoa(len(referrerBody)))
			_, _ = writer.Write(referrerBody)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerFallbackTag(subject)) && request.Method == http.MethodGet:
			mu.Lock()
			data := append([]byte(nil), fallbackIndex...)
			mu.Unlock()
			if len(data) == 0 {
				writeManifestMiss(writer)
				return
			}
			digest := artifact.CalculateSHA256(data)
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = writer.Write(data)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerFallbackTag(subject)) && request.Method == http.MethodPut:
			data, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "invalid body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			fallbackPutAttempts++
			attempt := fallbackPutAttempts
			if attempt > 1 {
				fallbackIndex = append([]byte(nil), data...)
			}
			mu.Unlock()
			if attempt == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(data))
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service, manager, registry, store, routed := newReferrerRepairTestService(t, server)
	defer manager.Close()
	defer registry.Close()
	defer store.Close()
	descriptor, err := buildReferrerDescriptor(referrerBody, ociManifestMediaType, referrer, artifact.Manifest{MediaType: ociManifestMediaType, Subject: &artifact.Descriptor{Digest: subject}})
	if err != nil {
		t.Fatal(err)
	}
	updateErr := service.updateFallbackReferrers(ctx, routed, subject, descriptor, false)
	var pending *referrerRepairPendingError
	if !errors.As(updateErr, &pending) {
		t.Fatal("initial fallback update unexpectedly succeeded")
	}
	intentKey := referrerRepairKey("ghcr.io", "team/image", subject, referrer)
	intent, err := service.loadReferrerRepair(ctx, intentKey)
	if err != nil {
		t.Fatalf("load repair intent after partial write: %v", err)
	}
	if !intent.Retryable || intent.Attempts != 1 {
		t.Fatalf("repair intent after partial write = %#v, want retryable attempt 1", intent)
	}
	intent.NextAttemptAt = time.Time{}
	if err := service.updateReferrerRepair(ctx, intent); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	registry.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	restartedRegistry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer restartedRegistry.Close()
	restartedManager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedManager.Close()
	restartedGate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{
		cfg: service.cfg, logger: service.logger, store: store, registry: restartedRegistry,
		tasks: restartedManager, gate: restartedGate, metrics: newMetrics(), solidificationSlots: make(chan struct{}, 4),
	}
	intent, err = restarted.loadReferrerRepair(ctx, intentKey)
	if err != nil {
		t.Fatalf("load repair intent after backend restart: %v", err)
	}
	if err := restarted.processReferrerRepair(ctx, intent); err != nil {
		t.Fatalf("replay fallback referrer repair: %v", err)
	}
	if _, err := restarted.loadReferrerRepair(ctx, intentKey); err == nil {
		t.Fatal("successful repair left its intent behind")
	}
	mu.Lock()
	indexBody := append([]byte(nil), fallbackIndex...)
	putAttempts := fallbackPutAttempts
	mu.Unlock()
	var index referrerDocument
	if err := json.Unmarshal(indexBody, &index); err != nil {
		t.Fatalf("decode repaired index: %v", err)
	}
	if len(index.Manifests) != 1 || !strings.Contains(string(index.Manifests[0]), referrer) || putAttempts != 2 {
		t.Fatalf("repaired index=%s PUT attempts=%d, want referrer and two attempts", indexBody, putAttempts)
	}
}
