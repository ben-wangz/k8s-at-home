package s3store

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCleanupOrphansSkipsActiveMultipartUpload(t *testing.T) {
	var aborts atomic.Int32
	initiated := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Query().Has("uploads") {
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<ListMultipartUploadsResult><Bucket>registry-hub</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker></NextKeyMarker><NextUploadIdMarker></NextUploadIdMarker><MaxUploads>1000</MaxUploads><IsTruncated>false</IsTruncated><Upload><Key>test/active</Key><UploadId>upload-1</UploadId><Initiated>%s</Initiated></Upload></ListMultipartUploadsResult>`, initiated)
			return
		}
		if request.Method == http.MethodDelete && request.URL.Query().Get("uploadId") == "upload-1" {
			aborts.Add(1)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()
	release, err := store.beginMultipart(context.Background(), "test/active")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	removed, err := store.CleanupOrphans(context.Background(), time.Hour, 100)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 0 || aborts.Load() != 0 {
		t.Fatalf("cleanup removed=%d aborts=%d for an active multipart upload", removed, aborts.Load())
	}
}

func TestMultipartCleanupClaimFencesNewUpload(t *testing.T) {
	store := &Store{}
	releaseCleanup := store.claimMultipartCleanup("test/key")
	if releaseCleanup == nil {
		t.Fatal("cleanup claim was not acquired")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.beginMultipart(ctx, "test/key"); err == nil {
		t.Fatal("upload began while orphan cleanup held the key")
	}
	releaseCleanup()

	releaseUpload, err := store.beginMultipart(context.Background(), "test/key")
	if err != nil {
		t.Fatalf("begin upload after cleanup claim release: %v", err)
	}
	releaseUpload()
}
