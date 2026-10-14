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

func TestWriteRegistryResponseAddsContentRangeForUnsatisfiedBlobRead(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		if request.Method != http.MethodHead {
			t.Errorf("internal range-size probe method = %s, want HEAD", request.Method)
		}
		writer.Header().Set("Content-Length", "6")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse internal Registry URL: %v", err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	defer registry.Close()

	response := &http.Response{
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"BLOB_UNKNOWN"}]}`)),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v2/acceptance/blobs/sha256:blob", nil)
	request.Header.Set("Range", "bytes=99-100")
	routed := route.Route{Endpoint: route.EndpointBlob, Repository: "acceptance", Internal: "acceptance", Reference: "sha256:blob"}

	(&Service{registry: registry}).writeRegistryResponse(recorder, request, routed, response, false)

	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusRequestedRangeNotSatisfiable)
	}
	if got := result.Header.Get("Content-Range"); got != "bytes */6" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes */6")
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Fatalf("internal requests = %v, want one HEAD", methods)
	}
}

func TestWriteRegistryResponsePreservesUploadRangeAfterInvalidPatch(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Header: http.Header{
			"Range":    []string{"0-16"},
			"Location": []string{"/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id?_state=opaque-token"},
		},
		Body: io.NopCloser(strings.NewReader(`{"errors":[{"code":"RANGE_INVALID","message":"internal"}]}`)),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id", nil)
	routed := route.Route{
		Endpoint: route.EndpointUpload, Repository: "docker.io/registry-hub-acceptance",
		Internal: "edocker_2eio/registry-hub-acceptance",
	}

	(&Service{}).writeRegistryResponse(recorder, request, routed, response, false)

	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusRequestedRangeNotSatisfiable)
	}
	if got := result.Header.Get("Range"); got != "0-16" {
		t.Fatalf("Range = %q, want %q", got, "0-16")
	}
	if got := result.Header.Get("Location"); got != response.Header.Get("Location") {
		t.Fatalf("Location = %q, want opaque session URL %q", got, response.Header.Get("Location"))
	}
	var envelope registryapi.ErrorEnvelope
	if err := json.NewDecoder(result.Body).Decode(&envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "RANGE_INVALID" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}

func TestWriteRegistryResponsePreservesUploadSessionLocation(t *testing.T) {
	routed := route.Route{
		Endpoint:   route.EndpointUpload,
		Repository: "docker.io/registry-hub-acceptance",
		Internal:   "edocker_2eio/registry-hub-acceptance",
	}
	for _, test := range []struct {
		method string
		status int
	}{
		{method: http.MethodPost, status: http.StatusAccepted},
		{method: http.MethodPatch, status: http.StatusNoContent},
	} {
		t.Run(test.method, func(t *testing.T) {
			location := "/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id?_state=session-token"
			response := &http.Response{
				StatusCode: test.status,
				Header: http.Header{
					"Location": []string{location},
					"Range":    []string{"0-10"},
				},
				Body: io.NopCloser(strings.NewReader("")),
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, "/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id", nil)

			(&Service{}).writeRegistryResponse(recorder, request, routed, response, false)

			result := recorder.Result()
			defer result.Body.Close()
			if result.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", result.StatusCode, test.status)
			}
			if got := result.Header.Get("Location"); got != location {
				t.Fatalf("Location = %q, want %q", got, location)
			}
			if got := result.Header.Get("Range"); got != "0-10" {
				t.Fatalf("Range = %q, want %q", got, "0-10")
			}
		})
	}
}

func TestIsUploadSessionLocationRejectsInternalOrMalformedLocations(t *testing.T) {
	for _, location := range []string{
		"/v2/edocker_2eio/registry-hub-acceptance/blobs/uploads/session-id?_state=token",
		"/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id",
		"/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id?_state=one&_state=two",
		"/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id/extra?_state=token",
		"https://registry.example/v2/docker.io/registry-hub-acceptance/blobs/uploads/session-id?_state=token",
	} {
		if isUploadSessionLocation(location, "docker.io/registry-hub-acceptance") {
			t.Errorf("isUploadSessionLocation(%q) = true, want false", location)
		}
	}
}

func TestWriteRegistryResponseClearsHeadersWhenLocationRewriteFails(t *testing.T) {
	registry, err := distribution.New(config.Config{DistributionURL: &url.URL{Scheme: "https", Host: "registry.internal"}})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	defer registry.Close()
	response := &http.Response{
		StatusCode: http.StatusAccepted,
		Header: http.Header{
			"Content-Length":     []string{"0"},
			"Docker-Upload-Uuid": []string{"internal-upload"},
			"Location":           []string{"/v2/other/repository/blobs/uploads/internal-upload?_state=secret"},
		},
		Body: io.NopCloser(strings.NewReader("")),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/docker.io/registry-hub-acceptance/blobs/uploads/", nil)
	routed := route.Route{
		Endpoint: route.EndpointUpload, Repository: "docker.io/registry-hub-acceptance",
		Internal: "edocker_2eio/registry-hub-acceptance",
	}

	(&Service{registry: registry}).writeRegistryResponse(recorder, request, routed, response, false)

	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusBadGateway)
	}
	if result.Header.Get("Content-Length") != "" || result.Header.Get("Docker-Upload-UUID") != "" {
		t.Fatalf("invalid Location error retained stale headers: %#v", result.Header)
	}
	var envelope registryapi.ErrorEnvelope
	if err := json.NewDecoder(result.Body).Decode(&envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNKNOWN" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}
