package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestServeLocalReferrersFallsBackOnPlainNativeNotFound(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	fallbackTag := referrerFallbackTag(subject)
	var fallbackRequested bool
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/referrers/"+subject):
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, "page not found\n")
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag):
			fallbackRequested = true
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
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
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	defer store.Close()

	service := &Service{
		cfg:      config.Config{Limits: config.Limits{MaxPageSize: 100, ManifestBytes: 1 << 20}},
		registry: registry,
		store:    store,
	}
	routed := route.Route{
		Repository: "local/repository",
		Internal:   "local/repository",
		Reference:  subject,
		Endpoint:   route.EndpointReferrers,
	}
	response, err := registry.Do(context.Background(), http.MethodGet, "/v2/local/repository/referrers/"+url.PathEscape(subject), nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		t.Fatalf("request native referrers: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v2/local/repository/referrers/"+subject, nil)
	recorder := httptest.NewRecorder()
	service.serveLocalReferrers(recorder, request, routed, make(url.Values), response, &requestMeta{})

	if !fallbackRequested {
		t.Fatal("fallback index was not requested after the native plain 404")
	}
	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", result.StatusCode, http.StatusOK, recorder.Body.String())
	}
	if got := result.Header.Get("Content-Type"); got != referrerIndexMediaType {
		t.Fatalf("Content-Type = %q, want %q", got, referrerIndexMediaType)
	}
	var document referrerDocument
	if err := json.NewDecoder(result.Body).Decode(&document); err != nil {
		t.Fatalf("decode referrers response: %v", err)
	}
	if document.SchemaVersion != 2 || document.MediaType != referrerIndexMediaType || document.Manifests == nil || len(document.Manifests) != 0 {
		t.Fatalf("unexpected empty referrers index: %#v", document)
	}
}

func TestServeLocalReferrersRejectsInvalidNativeIndexBeforeFallback(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	fallbackTag := referrerFallbackTag(subject)
	fallbackReads := 0
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/referrers/"+subject):
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "not-json")
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag):
			fallbackReads++
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer registryServer.Close()

	service, registry, store := newReferrerTestService(t, registryServer)
	defer registry.Close()
	defer store.Close()
	routed := route.Route{Repository: "local/repository", Internal: "local/repository", Reference: subject, Endpoint: route.EndpointReferrers}
	response := getNativeReferrers(t, registry, routed)
	recorder := httptest.NewRecorder()
	service.serveLocalReferrers(recorder, httptest.NewRequest(http.MethodGet, "/v2/local/repository/referrers/"+subject, nil), routed, make(url.Values), response, &requestMeta{})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if fallbackReads != 0 {
		t.Fatalf("fallback index reads = %d, want 0", fallbackReads)
	}
}

func TestServeLocalReferrersKeepsNativeEmptyIndexAuthoritative(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	fallbackTag := referrerFallbackTag(subject)
	fallbackReads := 0
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/referrers/"+subject):
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag):
			fallbackReads++
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","size":1}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer registryServer.Close()

	service, registry, store := newReferrerTestService(t, registryServer)
	defer registry.Close()
	defer store.Close()
	routed := route.Route{Repository: "local/repository", Internal: "local/repository", Reference: subject, Endpoint: route.EndpointReferrers}
	if _, err := service.storeReferrerMigration(context.Background(), "", routed.Repository, subject,
		"sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64), nil); err != nil {
		t.Fatalf("store completed migration state: %v", err)
	}
	response := getNativeReferrers(t, registry, routed)
	recorder := httptest.NewRecorder()
	service.serveLocalReferrers(recorder, httptest.NewRequest(http.MethodGet, "/v2/local/repository/referrers/"+subject, nil), routed, make(url.Values), response, &requestMeta{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var document referrerDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode referrers response: %v", err)
	}
	if len(document.Manifests) != 0 {
		t.Fatalf("native empty result was merged with fallback: %#v", document.Manifests)
	}
	if fallbackReads != 0 {
		t.Fatalf("fallback index reads = %d, want 0", fallbackReads)
	}
}

func TestServeLocalReferrersKeepsNativeNonEmptyIndexAuthoritative(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	nativeDigest := "sha256:" + strings.Repeat("b", 64)
	fallbackDigest := "sha256:" + strings.Repeat("c", 64)
	nativeIndex := `{"schemaVersion":2,"mediaType":"` + referrerIndexMediaType + `","manifests":[{"mediaType":"` + ociManifestMediaType + `","digest":"` + nativeDigest + `","size":1}]}`
	fallbackIndex := `{"schemaVersion":2,"mediaType":"` + referrerIndexMediaType + `","manifests":[{"mediaType":"` + ociManifestMediaType + `","digest":"` + fallbackDigest + `","size":1}]}`
	fallbackReads := 0
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/referrers/"+subject):
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, nativeIndex)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+referrerFallbackTag(subject)):
			fallbackReads++
			writer.Header().Set("Content-Type", referrerIndexMediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, fallbackIndex)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer registryServer.Close()

	service, registry, store := newReferrerTestService(t, registryServer)
	defer registry.Close()
	defer store.Close()
	routed := route.Route{Repository: "local/repository", Internal: "local/repository", Reference: subject, Endpoint: route.EndpointReferrers}
	if _, err := service.storeReferrerMigration(context.Background(), "", routed.Repository, subject,
		artifact.CalculateSHA256([]byte(fallbackIndex)), artifact.CalculateSHA256([]byte(nativeIndex)), []string{nativeDigest}); err != nil {
		t.Fatalf("store completed migration state: %v", err)
	}
	response := getNativeReferrers(t, registry, routed)
	recorder := httptest.NewRecorder()
	service.serveLocalReferrers(recorder,
		httptest.NewRequest(http.MethodGet, "/v2/local/repository/referrers/"+subject, nil),
		routed, make(url.Values), response, &requestMeta{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var document referrerDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Manifests) != 1 || !strings.Contains(string(document.Manifests[0]), nativeDigest) || strings.Contains(string(document.Manifests[0]), fallbackDigest) {
		t.Fatalf("native index was merged with fallback: %#v", document.Manifests)
	}
	if fallbackReads != 0 {
		t.Fatalf("fallback reads = %d, want 0", fallbackReads)
	}
}
