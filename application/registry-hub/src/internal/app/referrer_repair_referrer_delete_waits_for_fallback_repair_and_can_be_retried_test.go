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
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestReferrerDeleteWaitsForFallbackRepairAndCanBeRetried(t *testing.T) {
	ctx := context.Background()
	subjectBody := []byte("subject")
	subject := artifact.CalculateSHA256(subjectBody)
	emptyConfig := []byte("{}")
	referrerBody, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        &artifact.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: artifact.CalculateSHA256(emptyConfig), Size: int64(len(emptyConfig))},
		Layers:        []artifact.Descriptor{},
		Subject:       &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: subject, Size: int64(len(subjectBody))},
	})
	if err != nil {
		t.Fatal(err)
	}
	referrerDigest := artifact.CalculateSHA256(referrerBody)
	descriptor, err := buildReferrerDescriptor(referrerBody, ociManifestMediaType, referrerDigest, artifact.Manifest{
		Subject: &artifact.Descriptor{Digest: subject},
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackTag := referrerFallbackTag(subject)
	fallbackBody, err := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: []json.RawMessage{descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	manifestPresent := true
	fallbackPutAttempts := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"name":"ghcr.io/team/image","tags":[]}`)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerDigest) && request.Method == http.MethodGet:
			mu.Lock()
			present := manifestPresent
			mu.Unlock()
			if !present {
				writeManifestMiss(writer)
				return
			}
			writer.Header().Set("Content-Type", ociManifestMediaType)
			writer.Header().Set("Docker-Content-Digest", referrerDigest)
			writer.Header().Set("Content-Length", strconv.Itoa(len(referrerBody)))
			_, _ = writer.Write(referrerBody)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerDigest) && request.Method == http.MethodDelete:
			mu.Lock()
			manifestPresent = false
			mu.Unlock()
			writer.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag) && request.Method == http.MethodGet:
			mu.Lock()
			body := append([]byte(nil), fallbackBody...)
			mu.Unlock()
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(body))
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = writer.Write(body)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag) && request.Method == http.MethodPut:
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "invalid body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			fallbackPutAttempts++
			attempt := fallbackPutAttempts
			if attempt > 1 {
				fallbackBody = append([]byte(nil), body...)
			}
			mu.Unlock()
			if attempt == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(body))
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
	service.pushSlots = make(chan struct{}, 16)
	routed.Reference = referrerDigest
	routed.Endpoint = route.EndpointManifest
	request := httptest.NewRequest(http.MethodDelete, "/v2/ghcr.io/team/image/manifests/"+referrerDigest, nil)
	first := httptest.NewRecorder()
	service.deleteLocal(first, request, routed, make(url.Values), &requestMeta{})
	if first.Code != http.StatusServiceUnavailable || first.Header().Get("Retry-After") != "5" {
		t.Fatalf("first delete status=%d Retry-After=%q body=%s", first.Code, first.Header().Get("Retry-After"), first.Body.String())
	}
	mu.Lock()
	presentAfterFailure := manifestPresent
	mu.Unlock()
	if !presentAfterFailure {
		t.Fatal("referrer manifest was deleted before its fallback index update succeeded")
	}
	intentKey := referrerRepairKey("ghcr.io", "team/image", subject, referrerDigest)
	intent, err := service.loadReferrerRepair(ctx, intentKey)
	if err != nil {
		t.Fatalf("load delete repair intent: %v", err)
	}
	if intent.Operation != "remove" || !intent.DeleteManifest || !intent.Retryable || intent.Attempts != 1 {
		t.Fatalf("delete repair intent = %#v, want retryable remove with retained manifest", intent)
	}

	second := httptest.NewRecorder()
	service.deleteLocal(second, httptest.NewRequest(http.MethodDelete, request.URL.Path, nil), routed, make(url.Values), &requestMeta{})
	if second.Code != http.StatusAccepted {
		t.Fatalf("retry delete status=%d body=%s", second.Code, second.Body.String())
	}
	mu.Lock()
	presentAfterRetry := manifestPresent
	updatedIndex := append([]byte(nil), fallbackBody...)
	putAttempts := fallbackPutAttempts
	mu.Unlock()
	if presentAfterRetry || putAttempts != 2 {
		t.Fatalf("manifestPresent=%t fallback PUT attempts=%d, want deleted after a successful retry", presentAfterRetry, putAttempts)
	}
	var index referrerDocument
	if err := json.Unmarshal(updatedIndex, &index); err != nil {
		t.Fatalf("decode updated fallback index: %v", err)
	}
	if len(index.Manifests) != 0 {
		t.Fatalf("fallback index still contains a referrer after delete: %s", updatedIndex)
	}
	if _, err := service.loadReferrerRepair(ctx, intentKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("successful retry left a delete repair intent: %v", err)
	}
}

func TestReferrerRepairDoesNotOverwriteCorruptFallbackIndex(t *testing.T) {
	ctx := context.Background()
	subject := "sha256:" + strings.Repeat("a", 64)
	referrer := "sha256:" + strings.Repeat("b", 64)
	putAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.WriteHeader(http.StatusOK)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerFallbackTag(subject)):
			if request.Method == http.MethodPut {
				putAttempts++
				writer.WriteHeader(http.StatusCreated)
				return
			}
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "not-json")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service, manager, registry, store, _ := newReferrerRepairTestService(t, server)
	defer manager.Close()
	defer registry.Close()
	defer store.Close()
	routed, err := taskRoute("ghcr.io", "team/image")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.beginReferrerRepair(ctx, routed, subject, referrer, "add", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processReferrerRepair(ctx, intent); err == nil {
		t.Fatal("corrupt fallback index unexpectedly repaired")
	}
	if putAttempts != 0 {
		t.Fatalf("corrupt fallback index caused %d PUTs, want 0", putAttempts)
	}
	if _, err := service.loadReferrerRepair(ctx, referrerRepairKey("ghcr.io", "team/image", subject, referrer)); err != nil {
		t.Fatalf("corrupt index repair intent was not retained: %v", err)
	}
}
