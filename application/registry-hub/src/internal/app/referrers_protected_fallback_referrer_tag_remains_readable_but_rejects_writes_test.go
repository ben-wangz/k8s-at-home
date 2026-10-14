package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProtectedFallbackReferrerTagRemainsReadableButRejectsWrites(t *testing.T) {
	ctx := context.Background()
	subject := "sha256:" + strings.Repeat("a", 64)
	fallbackTag := referrerFallbackTag(subject)
	indexBody := []byte(ociIndexBody)
	indexDigest := artifact.CalculateSHA256(indexBody)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"name":"local/repository","tags":["`+fallbackTag+`"]}`)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag):
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.Header().Set("Docker-Content-Digest", indexDigest)
			writer.Header().Set("Content-Length", strconv.Itoa(len(indexBody)))
			if request.Method == http.MethodGet {
				_, _ = writer.Write(indexBody)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service, registry, store := newReferrerTestService(t, server)
	defer registry.Close()
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	service.tasks = manager
	service.metrics = newMetrics()
	service.pushSlots = make(chan struct{}, 16)
	service.gate, err = newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := route.Parse("/v2/local/repository/manifests/" + fallbackTag)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.recordFallbackMarker(ctx, routed, subject, fallbackTag, indexDigest); err != nil {
		t.Fatalf("record fallback tag marker: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, "/v2/local/repository/manifests/"+fallbackTag, nil)
		request.Header.Set("Accept", manifestAccept)
		recorder := httptest.NewRecorder()
		service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s protected fallback read = %d %s", method, recorder.Code, recorder.Body.String())
		}
	}

	tagsRoute, err := route.Parse("/v2/local/repository/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	tagsRecorder := httptest.NewRecorder()
	service.serveRead(tagsRecorder, httptest.NewRequest(http.MethodGet, tagsRoute.RawPath, nil), tagsRoute, make(url.Values), &requestMeta{})
	if tagsRecorder.Code != http.StatusOK || !strings.Contains(tagsRecorder.Body.String(), fallbackTag) {
		t.Fatalf("protected fallback tag listing = %d %s", tagsRecorder.Code, tagsRecorder.Body.String())
	}

	pushRequest := httptest.NewRequest(http.MethodPut, "/v2/local/repository/manifests/"+fallbackTag, strings.NewReader(ociManifestBody))
	pushRequest.Header.Set("Content-Type", ociManifestMediaType)
	pushRecorder := httptest.NewRecorder()
	service.pushManifest(pushRecorder, pushRequest, routed, make(url.Values), &requestMeta{})
	if pushRecorder.Code != http.StatusConflict {
		t.Fatalf("protected fallback tag PUT = %d %s, want 409", pushRecorder.Code, pushRecorder.Body.String())
	}
	indexPushRequest := httptest.NewRequest(http.MethodPut, "/v2/local/repository/manifests/"+indexDigest, strings.NewReader(string(indexBody)))
	indexPushRequest.Header.Set("Content-Type", referrerIndexMediaType)
	indexPushRoute, err := route.Parse(indexPushRequest.URL.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	indexPushRecorder := httptest.NewRecorder()
	service.pushManifest(indexPushRecorder, indexPushRequest, indexPushRoute, url.Values{"tag": []string{fallbackTag}}, &requestMeta{})
	if indexPushRecorder.Code != http.StatusConflict {
		t.Fatalf("digest PUT with protected tag query = %d %s, want 409", indexPushRecorder.Code, indexPushRecorder.Body.String())
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v2/local/repository/manifests/"+fallbackTag, nil)
	deleteRecorder := httptest.NewRecorder()
	service.deleteLocal(deleteRecorder, deleteRequest, routed, make(url.Values), &requestMeta{})
	if deleteRecorder.Code != http.StatusConflict {
		t.Fatalf("protected fallback tag DELETE = %d %s, want 409", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	indexDeleteRoute, err := route.Parse("/v2/local/repository/manifests/" + indexDigest)
	if err != nil {
		t.Fatal(err)
	}
	indexDeleteRequest := httptest.NewRequest(http.MethodDelete, indexDeleteRoute.RawPath, nil)
	indexDeleteRecorder := httptest.NewRecorder()
	service.deleteLocal(indexDeleteRecorder, indexDeleteRequest, indexDeleteRoute, make(url.Values), &requestMeta{})
	if indexDeleteRecorder.Code != http.StatusConflict {
		t.Fatalf("protected fallback index digest DELETE = %d %s, want 409", indexDeleteRecorder.Code, indexDeleteRecorder.Body.String())
	}
}

func TestNativeReferrerCapabilityCacheExpiresAndRejectsErrors(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	nativeStatus := http.StatusNotFound
	nativeHits := 0
	tagHits := 0
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			nativeHits++
			if nativeStatus == http.StatusOK {
				writer.Header().Set("Content-Type", referrerIndexMediaType)
				writer.WriteHeader(nativeStatus)
				_, _ = io.WriteString(writer, "not-json")
				return
			}
			writer.WriteHeader(nativeStatus)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			tagHits++
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer registryServer.Close()

	endpoint, err := url.Parse(registryServer.URL)
	if err != nil {
		t.Fatalf("parse test Registry URL: %v", err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	defer registry.Close()
	routed := route.Route{
		Repository: "registry.example/team/image", Internal: "registry.example/team/image",
		Upstream: "registry.example", RemoteRepo: "team/image", HasUpstream: true,
	}
	service := &Service{cfg: config.Config{Limits: config.Limits{ManifestBytes: 1 << 20, MaxDescriptors: 100, DescriptorDataBytes: 1 << 20}}, registry: registry}

	for range 2 {
		available, err := service.nativeReferrersAvailable(context.Background(), routed, subject)
		if err != nil || available {
			t.Fatalf("cached native 404 result = (%t, %v), want (false, nil)", available, err)
		}
	}
	key := routed.Upstream + "\x00" + routed.RemoteRepo
	if nativeHits != 1 || tagHits != 1 {
		t.Fatalf("native probes=%d repository confirmations=%d, want one each", nativeHits, tagHits)
	}
	service.nativeMu.Lock()
	entry, cached := service.nativeCache[key]
	service.nativeCache[key] = nativeReferrerCapability{available: true, expiresAt: time.Now().Add(-time.Second)}
	service.nativeMu.Unlock()
	remaining := time.Until(entry.expiresAt)
	if !cached || remaining <= 28*time.Second || remaining > 30*time.Second {
		t.Fatalf("native 404 cache remaining TTL = %s, want (28s, 30s]", remaining)
	}

	nativeStatus = http.StatusBadGateway
	if _, err := service.nativeReferrersAvailable(context.Background(), routed, subject); err == nil {
		t.Fatal("native 502 unexpectedly produced a capability result")
	}
	service.nativeMu.Lock()
	_, cached = service.nativeCache[key]
	service.nativeMu.Unlock()
	if cached {
		t.Fatal("native 502 left a capability cache entry")
	}

	nativeStatus = http.StatusOK
	if _, err := service.nativeReferrersAvailable(context.Background(), routed, subject); err == nil {
		t.Fatal("malformed native 200 unexpectedly produced a capability result")
	}
	service.nativeMu.Lock()
	_, cached = service.nativeCache[key]
	service.nativeMu.Unlock()
	if cached {
		t.Fatal("malformed native response left a capability cache entry")
	}
}
