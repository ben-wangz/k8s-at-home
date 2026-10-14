package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strings"
	"testing"
)

func TestBackendRejectsUntrustedInternalRegistryCAWithoutFallback(t *testing.T) {
	var registryRequests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		registryRequests++
		writer.WriteHeader(http.StatusNotFound)
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

	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK}}}
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{registry: registry, upstream: remote}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("untrusted internal Registry status = %d, want 502", recorder.Code)
	}
	if registryRequests != 0 {
		t.Fatalf("untrusted TLS endpoint received %d HTTP requests, want 0", registryRequests)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("internal TLS failure triggered %d upstream requests, want 0", len(remote.calls))
	}
	if strings.Contains(recorder.Body.String(), server.URL) || strings.Contains(recorder.Body.String(), "certificate") {
		t.Fatalf("internal TLS details leaked to client: %s", recorder.Body.String())
	}
}

func TestUnsafeDistributionRedirectIsSanitizedFromClientResponse(t *testing.T) {
	const redirectTarget = "http://127.0.0.1:1/object?signature=storage-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", redirectTarget)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{
		DistributionURL: endpoint, DistributionRedirectEndpoints: []string{"http://127.0.0.1:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{registry: registry}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("unsafe internal redirect status = %d, want 502", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "127.0.0.1") || strings.Contains(recorder.Body.String(), "signature") || strings.Contains(recorder.Body.String(), "storage-secret") || recorder.Header().Get("Location") != "" {
		t.Fatalf("unsafe redirect target leaked to client: headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
}

func TestInternalRegistryAuthFailuresAreRetriedOnceAndSanitized(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		challengeHost string
		wantRequests  int
	}{
		{name: "matching bearer challenge", status: http.StatusUnauthorized, challengeHost: "same-origin", wantRequests: 2},
		{name: "foreign bearer challenge", status: http.StatusUnauthorized, challengeHost: "https://internal.invalid", wantRequests: 1},
		{name: "forbidden", status: http.StatusForbidden, wantRequests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var authorization []string
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				authorization = append(authorization, request.Header.Get("Authorization"))
				if test.status == http.StatusUnauthorized {
					realm := test.challengeHost
					if realm == "same-origin" {
						realm = server.URL + "/token"
					}
					writer.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, "internal authentication detail internal-secret")
			}))
			defer server.Close()

			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := distribution.New(config.Config{
				DistributionURL:  endpoint,
				DistributionAuth: config.DistributionAuth{Token: "internal-secret"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{registry: registry}
			recorder := httptest.NewRecorder()
			service.serveRead(recorder, httptest.NewRequest(http.MethodGet, routed.RawPath, nil), routed, make(url.Values), &requestMeta{})

			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
			}
			if len(authorization) != test.wantRequests {
				t.Fatalf("Distribution request count = %d, want %d", len(authorization), test.wantRequests)
			}
			for _, value := range authorization {
				if value != "Bearer internal-secret" {
					t.Fatalf("Distribution Authorization = %q", value)
				}
			}
			if recorder.Header().Get("WWW-Authenticate") != "" || strings.Contains(recorder.Body.String(), "internal-secret") || strings.Contains(recorder.Body.String(), endpoint.Host) {
				t.Fatalf("internal authentication details leaked: headers=%v body=%s", recorder.Header(), recorder.Body.String())
			}
		})
	}
}

func TestWriteRegistryResponseReplacesStaleErrorHeaders(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusMethodNotAllowed,
		Header: http.Header{
			"Content-Length":        []string{"19"},
			"Content-Encoding":      []string{"gzip"},
			"Range":                 []string{"0-1"},
			"Content-Range":         []string{"bytes 0-0/1"},
			"Docker-Content-Digest": []string{"sha256:stale"},
			"Docker-Upload-Uuid":    []string{"stale-upload"},
			"Oci-Chunk-Min-Length":  []string{"1"},
			"Etag":                  []string{"stale-etag"},
			"Last-Modified":         []string{"Wed, 21 Oct 2015 07:28:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(`{"errors":[{"code":"UNSUPPORTED","message":"internal"}]}`)),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/registry.example/team/image/manifests/latest", nil)
	routed := route.Route{Endpoint: route.EndpointManifest, Repository: "registry.example/team/image", Internal: "eregistry_2eexample/team/image"}

	(&Service{}).writeRegistryResponse(recorder, request, routed, response, false)

	result := recorder.Result()
	defer result.Body.Close()
	for _, name := range []string{
		"Content-Length", "Content-Encoding", "Range", "Content-Range", "Accept-Ranges",
		"Docker-Content-Digest", "Docker-Upload-UUID", "OCI-Chunk-Min-Length", "ETag", "Last-Modified",
	} {
		if value := result.Header.Get(name); value != "" {
			t.Errorf("error response retained stale %s header %q", name, value)
		}
	}
	if result.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusMethodNotAllowed)
	}
	var envelope registryapi.ErrorEnvelope
	if err := json.NewDecoder(result.Body).Decode(&envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNSUPPORTED" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}
