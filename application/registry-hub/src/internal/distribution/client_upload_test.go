package distribution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/config"
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

func TestCancelUploadUsesCallerDeadline(t *testing.T) {
	started := make(chan struct{})
	client := newUploadCancellationTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := client.CancelUpload(ctx, "/v2/repo/blobs/uploads/session?_state=state", "repo")
	if err == nil {
		t.Fatal("CancelUpload() ignored its caller deadline")
	}
	select {
	case <-started:
	default:
		t.Fatal("cancellation request did not reach the server")
	}
}

func TestCancelUploadAgainstDistribution(t *testing.T) {
	endpoint := os.Getenv("REGISTRY_HUB_DISTRIBUTION_TEST_URL")
	if endpoint == "" {
		t.Skip("set REGISTRY_HUB_DISTRIBUTION_TEST_URL to an isolated Distribution 3.1.2 endpoint")
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse Distribution endpoint: %v", err)
	}
	client, err := New(config.Config{DistributionURL: base})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	defer client.Close()

	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	repository := "registry-hub-upload-cancel-" + hex.EncodeToString(random[:])
	uploadCollection := "/v2/" + repository + "/blobs/uploads/"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := client.Do(ctx, http.MethodPost, uploadCollection, nil, make(http.Header), nil, 0)
	if err != nil {
		closeResponse(created)
		t.Fatalf("create upload session: %v", err)
	}
	if created.StatusCode != http.StatusAccepted {
		closeResponse(created)
		t.Fatalf("upload creation status = %d, want 202", created.StatusCode)
	}
	initialLocation := created.Header.Get("Location")
	closeResponse(created)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = client.CancelUpload(cleanupCtx, initialLocation, repository)
	}()

	uploadPath, query, err := client.ParseLocation(initialLocation, repository)
	if err != nil {
		t.Fatalf("parse initial upload Location: %v", err)
	}
	partial := []byte("partial")
	patch, err := client.Do(ctx, http.MethodPatch, uploadPath, query, http.Header{
		"Content-Type":  []string{"application/octet-stream"},
		"Content-Range": []string{"0-6"},
	}, strings.NewReader(string(partial)), int64(len(partial)))
	if err != nil {
		t.Fatalf("write partial upload: %v", err)
	}
	patchStatus := patch.StatusCode
	closeResponse(patch)
	if patchStatus != http.StatusAccepted {
		t.Fatalf("partial upload status = %d, want 202", patchStatus)
	}

	if err := client.CancelUpload(ctx, initialLocation, repository); err != nil {
		t.Fatalf("cancel using the stale initial Location: %v", err)
	}
	status, err := client.Do(ctx, http.MethodGet, uploadPath, nil, make(http.Header), nil, 0)
	if err != nil {
		t.Fatalf("read status after cancellation: %v", err)
	}
	statusCode := status.StatusCode
	closeResponse(status)
	if statusCode != http.StatusNotFound {
		t.Fatalf("upload status after cancellation = %d, want 404", statusCode)
	}
}

func newUploadCancellationTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}
