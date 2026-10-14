package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strings"
	"testing"
	"time"
)

func TestHandleClientV2AuthenticationResponses(t *testing.T) {
	service := &Service{
		cfg: config.Config{
			ClientAuth: config.Auth{Basic: []config.Credential{{
				Username:    "acceptance-client",
				Password:    "acceptance-password",
				Permissions: []string{auth.Pull},
			}}},
			Limits: config.Limits{RequestTimeout: time.Second},
		},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics:     newMetrics(),
		limiter:     newRequestLimiter(100, 100, 100, 100),
		clientSlots: make(chan struct{}, 128),
		noAuthSlots: make(chan struct{}, 16),
	}

	unauthenticated := httptest.NewRecorder()
	service.ClientHandler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v2/ status = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}
	challenge := unauthenticated.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `Basic realm="registry-hub"`) || !strings.Contains(challenge, `Bearer realm="registry-hub"`) {
		t.Fatalf("unexpected /v2/ challenge %q", challenge)
	}
	var errorEnvelope registryapi.ErrorEnvelope
	if err := json.Unmarshal(unauthenticated.Body.Bytes(), &errorEnvelope); err != nil {
		t.Fatalf("unauthenticated /v2/ error is not valid JSON: %v", err)
	}
	if len(errorEnvelope.Errors) != 1 || errorEnvelope.Errors[0].Code != "UNAUTHORIZED" {
		t.Fatalf("unexpected /v2/ error envelope: %#v", errorEnvelope)
	}

	request := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	request.SetBasicAuth("acceptance-client", "acceptance-password")
	authenticated := httptest.NewRecorder()
	service.ClientHandler().ServeHTTP(authenticated, request)
	if authenticated.Code != http.StatusOK {
		t.Fatalf("authenticated /v2/ status = %d, want %d", authenticated.Code, http.StatusOK)
	}
	if got := authenticated.Header().Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("authenticated /v2/ API version = %q", got)
	}
}

func TestConfirmedMissOnlyAcceptsRegistryNotFoundStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	} {
		response := &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)),
		}
		if confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
			t.Errorf("confirmedMiss(status %d) = true, want false", status)
		}
		_ = response.Body.Close()
	}

	validMiss := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)),
	}
	if !confirmedMiss(validMiss, http.MethodGet, route.EndpointManifest) {
		t.Fatal("confirmedMiss(MANIFEST_UNKNOWN) = false, want true")
	}
	invalidMiss := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"BLOB_UNKNOWN"}]}`)),
	}
	if confirmedMiss(invalidMiss, http.MethodGet, route.EndpointManifest) {
		t.Fatal("confirmedMiss(BLOB_UNKNOWN for a manifest) = true, want false")
	}

	for _, test := range []struct {
		name     string
		endpoint route.Endpoint
		code     string
		want     bool
	}{
		{name: "manifest unknown", endpoint: route.EndpointManifest, code: "MANIFEST_UNKNOWN", want: true},
		{name: "manifest name unknown", endpoint: route.EndpointManifest, code: "NAME_UNKNOWN", want: true},
		{name: "blob unknown", endpoint: route.EndpointBlob, code: "BLOB_UNKNOWN", want: true},
		{name: "blob name unknown", endpoint: route.EndpointBlob, code: "NAME_UNKNOWN", want: true},
		{name: "tag manifest unknown", endpoint: route.EndpointTags, code: "MANIFEST_UNKNOWN", want: true},
		{name: "referrer name unknown", endpoint: route.EndpointReferrers, code: "NAME_UNKNOWN", want: true},
		{name: "wrong endpoint error", endpoint: route.EndpointBlob, code: "MANIFEST_UNKNOWN", want: false},
		{name: "unknown registry error", endpoint: route.EndpointManifest, code: "UNAUTHORIZED", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"` + test.code + `"}]}`)),
			}
			if got := confirmedMiss(response, http.MethodGet, test.endpoint); got != test.want {
				t.Fatalf("confirmedMiss(%s, %s) = %t, want %t", test.endpoint, test.code, got, test.want)
			}
			_ = response.Body.Close()
		})
	}

	headMiss := &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not json"))}
	if !confirmedMiss(headMiss, http.MethodHead, route.EndpointManifest) {
		t.Fatal("confirmedMiss(HEAD 404) = false, want true")
	}
	_ = headMiss.Body.Close()
}

func TestInternalRegistryErrorsDoNotFallback(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, `{"errors":[{"code":"UNKNOWN","message":"private internal detail"}]}`)
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

			if recorder.Code != status {
				t.Fatalf("status = %d, want %d", recorder.Code, status)
			}
			if len(remote.calls) != 0 {
				t.Fatalf("internal status triggered %d upstream requests, want 0", len(remote.calls))
			}
			if requests != 1 {
				t.Fatalf("internal Registry requests = %d, want 1", requests)
			}
			if strings.Contains(recorder.Body.String(), "private internal detail") {
				t.Fatalf("internal Registry error leaked to client: %s", recorder.Body.String())
			}
		})
	}
}

func TestInternalRegistryConnectionFailureDoesNotFallback(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	server.Close()

	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{registry: registry}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}
