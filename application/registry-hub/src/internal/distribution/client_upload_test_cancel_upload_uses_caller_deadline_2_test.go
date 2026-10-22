package distribution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"registry-hub/internal/config"
	"strings"
	"testing"
	"time"
)

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
