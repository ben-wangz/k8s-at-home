package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"registry-hub/internal/route"
)

func TestDeleteLocalPreservesDistributionDeleteSemantics(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name       string
		path       string
		deleteCode int
		wantCode   int
	}{
		{name: "existing tag", path: "/v2/ghcr.io/team/image/manifests/latest", deleteCode: http.StatusAccepted, wantCode: http.StatusAccepted},
		{name: "missing tag", path: "/v2/ghcr.io/team/image/manifests/missing", deleteCode: http.StatusNotFound, wantCode: http.StatusNotFound},
		{name: "deletion disabled", path: "/v2/ghcr.io/team/image/manifests/latest", deleteCode: http.StatusMethodNotAllowed, wantCode: http.StatusMethodNotAllowed},
		{name: "existing blob", path: "/v2/ghcr.io/team/image/blobs/" + digest, deleteCode: http.StatusAccepted, wantCode: http.StatusAccepted},
		{name: "missing blob", path: "/v2/ghcr.io/team/image/blobs/" + digest, deleteCode: http.StatusNotFound, wantCode: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			deleteRequests := 0
			verifyRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodDelete {
					deleteRequests++
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(test.deleteCode)
					if test.deleteCode == http.StatusNotFound {
						code := "MANIFEST_UNKNOWN"
						if strings.Contains(request.URL.Path, "/blobs/") {
							code = "BLOB_UNKNOWN"
						}
						_, _ = io.WriteString(writer, `{"errors":[{"code":"`+code+`"}]}`)
					}
					return
				}
				if request.Method == http.MethodGet {
					verifyRequests++
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusNotFound)
					code := "MANIFEST_UNKNOWN"
					if strings.Contains(request.URL.Path, "/blobs/") {
						code = "BLOB_UNKNOWN"
					}
					_, _ = io.WriteString(writer, `{"errors":[{"code":"`+code+`"}]}`)
					return
				}
				http.NotFound(writer, request)
			}))
			defer server.Close()

			service, manager, registry, store, _ := newReferrerRepairTestService(t, server)
			defer manager.Close()
			defer registry.Close()
			defer store.Close()
			service.metrics = newMetrics()
			service.pushSlots = make(chan struct{}, 4)
			routed, err := route.Parse(test.path)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodDelete, test.path, nil)
			response := httptest.NewRecorder()
			service.deleteLocal(response, request, routed, make(url.Values), &requestMeta{})
			if response.Code != test.wantCode {
				t.Fatalf("DELETE status = %d, want %d: %s", response.Code, test.wantCode, response.Body.String())
			}
			if deleteRequests != 1 {
				t.Fatalf("Distribution DELETE requests = %d, want 1", deleteRequests)
			}
			wantVerify := 0
			if test.deleteCode == http.StatusAccepted {
				wantVerify = 1
			}
			if verifyRequests != wantVerify {
				t.Fatalf("post-delete verification GETs = %d, want %d", verifyRequests, wantVerify)
			}
		})
	}
}

func TestDeleteManifestDigestDoesNotRefuseOrdinaryTagReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		if request.Method == http.MethodGet {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	service, manager, registry, store, _ := newReferrerRepairTestService(t, server)
	defer manager.Close()
	defer registry.Close()
	defer store.Close()
	service.metrics = newMetrics()
	service.pushSlots = make(chan struct{}, 4)
	path := "/v2/ghcr.io/team/image/manifests/" + digest
	routed, err := route.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, path, nil)
	response := httptest.NewRecorder()
	service.deleteLocal(response, request, routed, url.Values{"tag": []string{"stable"}}, &requestMeta{})
	if response.Code != http.StatusAccepted {
		t.Fatalf("manifest digest DELETE = %d, want 202: %s", response.Code, response.Body.String())
	}
}
