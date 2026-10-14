package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReferrerMigrationDrainsWritesAndRetriesWithoutLosingReferrers(t *testing.T) {
	ctx := context.Background()
	subjectBody := []byte("subject")
	subject := artifact.CalculateSHA256(subjectBody)
	referrerBody, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Subject:       &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: subject, Size: int64(len(subjectBody))},
	})
	if err != nil {
		t.Fatal(err)
	}
	referrer := artifact.CalculateSHA256(referrerBody)
	fallbackDescriptor, err := json.Marshal(map[string]any{
		"mediaType": ociManifestMediaType, "digest": referrer, "size": len(referrerBody),
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackIndex, err := json.Marshal(referrerDocument{
		SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: []json.RawMessage{fallbackDescriptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackTag := referrerFallbackTag(subject)
	var mu sync.Mutex
	var nativeDescriptors []json.RawMessage
	nativePuts := 0
	firstPut := make(chan struct{}, 1)
	finishFirstPut := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			mu.Lock()
			manifests := append([]json.RawMessage(nil), nativeDescriptors...)
			mu.Unlock()
			writeMigrationIndex(writer, manifests)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"name":"ghcr.io/team/image","tags":[]}`))
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag) && request.Method == http.MethodGet:
			writeMigrationManifest(writer, fallbackIndex, referrerIndexMediaType)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrer) && request.Method == http.MethodGet:
			writeMigrationManifest(writer, referrerBody, ociManifestMediaType)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrer) && request.Method == http.MethodPut:
			mu.Lock()
			nativePuts++
			attempt := nativePuts
			mu.Unlock()
			if attempt == 1 {
				firstPut <- struct{}{}
				<-finishFirstPut
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			nativeDescriptors = []json.RawMessage{append(json.RawMessage(nil), fallbackDescriptor...)}
			mu.Unlock()
			writer.Header().Set("Docker-Content-Digest", referrer)
			writer.Header().Set("OCI-Subject", subject)
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
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
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		cfg: config.Config{Limits: config.Limits{
			ManifestBytes: 1 << 20, DescriptorDataBytes: 1 << 20, MaxDescriptors: 100,
			MaxPageSize: 100, MaxReferrerPages: 10,
		}},
		registry: registry, store: store, tasks: manager, gate: gate,
	}
	routed, err := taskRoute("ghcr.io", "team/image")
	if err != nil {
		t.Fatal(err)
	}

	releaseExisting, err := service.gate.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	migrationResult := make(chan error, 1)
	go func() {
		_, migrateErr := service.migrateFallbackReferrers(ctx, routed, subject)
		migrationResult <- migrateErr
	}()
	waitForExclusiveGate(t, service.gate)
	if _, err := service.gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("new write admission while draining returned %v, want ErrWritesClosed", err)
	}
	referrersRoute, err := route.Parse("/v2/ghcr.io/team/image/referrers/" + subject)
	if err != nil {
		t.Fatal(err)
	}
	readRequest := httptest.NewRequest(http.MethodGet, referrersRoute.RawPath, nil)
	readResponse := httptest.NewRecorder()
	service.serveRead(readResponse, readRequest, referrersRoute, make(url.Values), &requestMeta{})
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), referrer) {
		t.Fatalf("Referrers read during migration drain = %d %s, want existing fallback referrer", readResponse.Code, readResponse.Body.String())
	}

	releaseExisting()
	select {
	case <-firstPut:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach native manifest PUT")
	}
	if _, err := service.gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("new write admission during native publish returned %v, want ErrWritesClosed", err)
	}
	close(finishFirstPut)
	select {
	case err := <-migrationResult:
		if err == nil || !referrerRetryable(err) {
			t.Fatalf("first migration error = %v, want retryable failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed migration did not return")
	}

	releaseAfterFailure, err := service.gate.Enter(ctx)
	if err != nil {
		t.Fatalf("write admission was not restored after migration failure: %v", err)
	}
	releaseAfterFailure()
	authority, repository := routeStorageIdentity(routed)
	pending, err := service.loadReferrerMigration(ctx, authority, repository, subject)
	if err != nil || pending.State != referrerMigrationMigrating || len(pending.Referrers) != 1 || pending.Referrers[0] != referrer {
		t.Fatalf("failed migration state = %#v, err=%v; fallback referrer was not retained", pending, err)
	}
	stillPending, err := service.referrerMigrationPending(ctx, routed, subject, true)
	if err != nil || !stillPending {
		t.Fatalf("failed migration pending check = (%t, %v), want (true, nil)", stillPending, err)
	}

	completed, err := service.migrateFallbackReferrers(ctx, routed, subject)
	if err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if completed.State != referrerMigrationComplete || len(completed.Referrers) != 1 || completed.Referrers[0] != referrer {
		t.Fatalf("completed migration = %#v, want the original referrer", completed)
	}
	repeated, err := service.migrateFallbackReferrers(ctx, routed, subject)
	if err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if repeated.Generation != completed.Generation {
		t.Fatalf("repeated migration generation = %d, want stable generation %d", repeated.Generation, completed.Generation)
	}
	mu.Lock()
	nativeDescriptors = nil
	mu.Unlock()
	restored, err := service.migrateFallbackReferrers(ctx, routed, subject)
	if err != nil {
		t.Fatalf("migration after native index loss: %v", err)
	}
	if restored.Generation <= repeated.Generation || len(restored.Referrers) != 1 || restored.Referrers[0] != referrer {
		t.Fatalf("migration after native index loss = %#v, want a new verified generation with the fallback referrer", restored)
	}
	mu.Lock()
	putCount := nativePuts
	nativeCount := len(nativeDescriptors)
	mu.Unlock()
	if putCount != 3 || nativeCount != 1 {
		t.Fatalf("native PUTs=%d native referrers=%d, want one failed PUT, one retry, one restoration, and one referrer", putCount, nativeCount)
	}
	releaseAfterSuccess, err := service.gate.Enter(ctx)
	if err != nil {
		t.Fatalf("write admission was not restored after successful migration: %v", err)
	}
	releaseAfterSuccess()
}

func writeMigrationManifest(writer http.ResponseWriter, body []byte, mediaType string) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(body))
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = writer.Write(body)
}

func writeMigrationIndex(writer http.ResponseWriter, manifests []json.RawMessage) {
	if manifests == nil {
		manifests = []json.RawMessage{}
	}
	body, err := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: manifests})
	if err != nil {
		http.Error(writer, "index encoding failed", http.StatusInternalServerError)
		return
	}
	writeMigrationManifest(writer, body, referrerIndexMediaType)
}
