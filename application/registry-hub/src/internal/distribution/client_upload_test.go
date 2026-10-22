package distribution

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestCancelUploadRefreshesStateAfterPartialWrite(t *testing.T) {
	const uploadPath = "/v2/repo/blobs/uploads/session"
	active := true
	partialBytes := 5
	var deleteStates []string
	var statusReads int
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != uploadPath {
			t.Errorf("request path = %q, want %q", request.URL.Path, uploadPath)
		}
		if request.URL.Query().Get("digest") != "" {
			t.Error("cancellation request included the blob commit digest")
		}
		switch request.Method {
		case http.MethodDelete:
			state := request.URL.Query().Get("_state")
			deleteStates = append(deleteStates, state)
			if state != "offset=5" {
				writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			active = false
			partialBytes = 0
			writer.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			statusReads++
			if !active {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Location", uploadPath+"?_state=offset%3D5")
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request method %q", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	initial := uploadPath + "?_state=initial%2Btoken%26value%3D1"
	if err := client.CancelUpload(context.Background(), initial, "repo"); err != nil {
		t.Fatalf("CancelUpload() error = %v", err)
	}
	if active || partialBytes != 0 {
		t.Fatalf("upload remains active=%t partialBytes=%d", active, partialBytes)
	}
	if statusReads != 1 || len(deleteStates) != 2 || deleteStates[0] != "initial+token&value=1" || deleteStates[1] != "offset=5" {
		t.Fatalf("status reads=%d delete states=%q, want one refresh and stale/current tokens", statusReads, deleteStates)
	}
}

func TestCancelUploadTreatsMissingSessionAsSuccess(t *testing.T) {
	var deletes int
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete {
			t.Errorf("request method = %q, want DELETE", request.Method)
		}
		deletes++
		writer.WriteHeader(http.StatusNotFound)
	}))
	if err := client.CancelUpload(context.Background(), "/v2/repo/blobs/uploads/session?_state=state", "repo"); err != nil {
		t.Fatalf("CancelUpload() for absent upload error = %v", err)
	}
	if deletes != 1 {
		t.Fatalf("DELETE requests = %d, want 1", deletes)
	}
}

func TestCancelUploadRefreshesRejectedState(t *testing.T) {
	const uploadPath = "/v2/repo/blobs/uploads/session"
	var deletes, refreshes int
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodDelete:
			deletes++
			if request.URL.Query().Get("_state") != "fresh" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			refreshes++
			writer.Header().Set("Location", uploadPath+"?_state=fresh")
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request method %q", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	if err := client.CancelUpload(context.Background(), uploadPath+"?_state=old", "repo"); err != nil {
		t.Fatalf("CancelUpload() error = %v", err)
	}
	if deletes != 2 || refreshes != 1 {
		t.Fatalf("DELETEs=%d refreshes=%d, want 2 and 1", deletes, refreshes)
	}
}

func TestCancelUploadBoundsStateRefreshes(t *testing.T) {
	const uploadPath = "/v2/repo/blobs/uploads/session"
	var deletes, refreshes int
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodDelete:
			deletes++
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		case http.MethodGet:
			refreshes++
			writer.Header().Set("Location", uploadPath+"?_state=offset%3D"+url.QueryEscape(strings.Repeat("x", refreshes)))
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request method %q", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	err := client.CancelUpload(context.Background(), uploadPath+"?_state=initial", "repo")
	if err == nil {
		t.Fatal("CancelUpload() succeeded despite repeated stale state")
	}
	if deletes != maxUploadCancelAttempts || refreshes != maxUploadCancelAttempts-1 {
		t.Fatalf("DELETEs=%d refreshes=%d, want at most %d and %d", deletes, refreshes, maxUploadCancelAttempts, maxUploadCancelAttempts-1)
	}
}

func TestCancelUploadRejectsChangedSessionIdentity(t *testing.T) {
	const uploadPath = "/v2/repo/blobs/uploads/session"
	deletes := 0
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodDelete:
			deletes++
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		case http.MethodGet:
			writer.Header().Set("Location", "/v2/repo/blobs/uploads/other?_state=fresh")
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request method %q", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	if err := client.CancelUpload(context.Background(), uploadPath+"?_state=old", "repo"); err == nil {
		t.Fatal("CancelUpload() accepted a changed upload identity")
	}
	if deletes != 1 {
		t.Fatalf("DELETE requests = %d, want 1; a second upload must not be cancelled", deletes)
	}
}

func TestCancelUploadRejectsInvalidLocationsWithoutRequests(t *testing.T) {
	var requests int
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.WriteHeader(http.StatusNoContent)
	}))
	for _, location := range []string{
		"",
		"https://outside.example/v2/repo/blobs/uploads/id?_state=x",
		"//outside.example/v2/repo/blobs/uploads/id?_state=x",
		"/v2/other/blobs/uploads/id?_state=x",
		"/v2/repo/blobs/uploads/",
		"/v2/repo/blobs/uploads/id#fragment",
	} {
		if err := client.CancelUpload(context.Background(), location, "repo"); err == nil {
			t.Errorf("CancelUpload(%q) succeeded for invalid location", location)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid locations caused %d network requests", requests)
	}
}
