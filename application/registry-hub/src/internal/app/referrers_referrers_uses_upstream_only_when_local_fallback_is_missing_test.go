package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strconv"
	"strings"
	"testing"
)

func TestReferrersUsesUpstreamOnlyWhenLocalFallbackIsMissing(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	remoteReferrerDigest := "sha256:" + strings.Repeat("f", 64)
	upstreamIndex := `{"schemaVersion":2,"mediaType":"` + referrerIndexMediaType + `","manifests":[{"mediaType":"` + ociManifestMediaType + `","digest":"` + remoteReferrerDigest + `","size":1}]}`
	for _, test := range []struct {
		name            string
		fallbackStatus  int
		wantUpstreamHit int
	}{
		{name: "missing fallback queries upstream", fallbackStatus: http.StatusNotFound, wantUpstreamHit: 1},
		{name: "empty fallback remains authoritative", fallbackStatus: http.StatusOK, wantUpstreamHit: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, contentType: referrerIndexMediaType, body: upstreamIndex}}}
			service, manager, _ := newDiscoveryTestService(t, remote)
			service.cfg.Limits.MaxPageSize = 100
			service.cfg.Limits.MaxReferrerPages = 10
			fallbackTag := referrerFallbackTag(subject)
			service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case strings.Contains(request.URL.Path, "/referrers/"):
					writeManifestMiss(writer)
				case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag):
					if test.fallbackStatus == http.StatusNotFound {
						writeManifestMiss(writer)
						return
					}
					writer.Header().Set("Content-Type", referrerIndexMediaType)
					_, _ = io.WriteString(writer, ociIndexBody)
				default:
					http.NotFound(writer, request)
				}
			}))

			routed, err := route.Parse("/v2/ghcr.io/team/image/referrers/" + subject)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
			recorder := httptest.NewRecorder()
			service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
			if recorder.Code != http.StatusOK {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			if len(remote.calls) != test.wantUpstreamHit {
				t.Fatalf("upstream calls = %d, want %d", len(remote.calls), test.wantUpstreamHit)
			}
			if got := taskCount(t, manager); got != 0 {
				t.Fatalf("Referrers discovery created %d solidification tasks", got)
			}
			if test.wantUpstreamHit > 0 {
				if _, err := service.cache.Get(context.Background(), "ghcr.io", "team/image", remoteReferrerDigest, ociManifestMediaType); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("upstream referrer cache lookup error = %v, want not found", err)
				}
				if !strings.Contains(recorder.Body.String(), remoteReferrerDigest) {
					t.Fatalf("upstream referrer discovery response omitted descriptor: %s", recorder.Body.String())
				}
			}
		})
	}
}

func newReferrerTestService(t *testing.T, server *httptest.Server) (*Service, *distribution.Client, *filesystem.Store) {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test Registry URL: %v", err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	service := &Service{cfg: config.Config{Limits: config.Limits{
		ManifestBytes: 1 << 20, DescriptorDataBytes: 1 << 20, MaxDescriptors: 100, MaxPageSize: 100, MaxReferrerPages: 10,
	}}, registry: registry, store: store}
	return service, registry, store
}

func getNativeReferrers(t *testing.T, registry *distribution.Client, routed route.Route) *http.Response {
	t.Helper()
	response, err := registry.Do(context.Background(), http.MethodGet,
		"/v2/"+routed.Internal+"/referrers/"+url.PathEscape(routed.Reference), nil,
		http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		t.Fatalf("request native referrers: %v", err)
	}
	return response
}

func TestNativeReferrerMissOnlyAcceptsNotFound(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway} {
		if nativeReferrerMiss(&http.Response{StatusCode: status}) {
			t.Errorf("nativeReferrerMiss(status %d) = true", status)
		}
	}
	if !nativeReferrerMiss(&http.Response{StatusCode: http.StatusNotFound}) {
		t.Fatal("nativeReferrerMiss(404) = false")
	}
}

func TestServeLocalReferrersDoesNotFallbackForNativeErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			subject := "sha256:" + strings.Repeat("a", 64)
			fallbackReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case strings.Contains(request.URL.Path, "/referrers/"):
					writer.WriteHeader(status)
				case strings.Contains(request.URL.Path, "/manifests/"):
					fallbackReads++
					writeManifestMiss(writer)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			service, registry, store := newReferrerTestService(t, server)
			defer registry.Close()
			defer store.Close()
			routed := route.Route{
				Repository: "local/repository", Internal: "local/repository",
				Reference: subject, Endpoint: route.EndpointReferrers,
			}
			response := getNativeReferrers(t, registry, routed)
			recorder := httptest.NewRecorder()
			service.serveLocalReferrers(recorder,
				httptest.NewRequest(http.MethodGet, "/v2/local/repository/referrers/"+subject, nil),
				routed, make(url.Values), response, &requestMeta{})

			if fallbackReads != 0 {
				t.Fatalf("fallback index reads = %d, want 0 for native status %d", fallbackReads, status)
			}
			if recorder.Code == http.StatusOK {
				t.Fatalf("native status %d was converted to a successful fallback response", status)
			}
		})
	}
}

func TestReferrerFallbackTagUsesOCISchemaRules(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	if got, want := referrerFallbackTag(digest), "sha256-"+strings.Repeat("a", 64); got != want {
		t.Fatalf("fallback tag = %q, want %q", got, want)
	}
	longAlgorithm := strings.Repeat("a", 40)
	longEncoded := strings.Repeat("b", 70)
	if got, want := referrerFallbackTag(longAlgorithm+":"+longEncoded), strings.Repeat("a", 32)+"-"+strings.Repeat("b", 64); got != want {
		t.Fatalf("truncated fallback tag = %q, want %q", got, want)
	}
	if got, want := referrerFallbackTag("sha+256:ab/c?"), "sha-256-ab-c-"; got != want {
		t.Fatalf("sanitized fallback tag = %q, want %q", got, want)
	}
}
