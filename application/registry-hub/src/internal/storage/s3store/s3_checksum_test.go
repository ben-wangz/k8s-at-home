package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"testing"
)

func TestSinglePutRequestsAndPersistsVerifiedS3Checksum(t *testing.T) {
	data := []byte("checksum-supported object")
	sum := sha256.Sum256(data)
	wantChecksum := base64.StdEncoding.EncodeToString(sum[:])
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || !bytes.Equal(body, data) {
			http.Error(writer, "invalid object", http.StatusBadRequest)
			return
		}
		received = request.Header.Get("x-amz-checksum-sha256")
		writer.Header().Set("ETag", `"checksum-etag"`)
		writer.Header().Set("x-amz-checksum-sha256", wantChecksum)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	store.checksumSHA256 = true
	defer store.Close()
	entry, err := store.Put(context.Background(), "test/checksum", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if received != wantChecksum || entry.ChecksumAlgorithm != "sha256" || entry.ChecksumValue != wantChecksum || entry.ChecksumScope != "complete_object" {
		t.Fatalf("request checksum=%q entry=%+v, want checksum %q", received, entry, wantChecksum)
	}
}

func TestS3ChecksumSupportDetectionIsConservative(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		want     bool
	}{
		{endpoint: "", want: true}, {endpoint: "https://s3.us-east-1.amazonaws.com", want: true},
		{endpoint: "https://s3.us-gov-west-1.amazonaws.com", want: true}, {endpoint: "https://s3.example.test", want: false}, {endpoint: "http://localhost:9000", want: false},
	} {
		if got := supportsAWSChecksumSHA256(test.endpoint); got != test.want {
			t.Errorf("supportsAWSChecksumSHA256(%q)=%t, want %t", test.endpoint, got, test.want)
		}
	}
}

func TestChecksumReadbackFallsBackWhenChecksumModeIsDenied(t *testing.T) {
	payload := []byte("object checksum mode is unavailable")
	var headRequests, getRequests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-amz-checksum-mode") == "ENABLED" {
			if request.Method == http.MethodHead {
				headRequests++
			} else if request.Method == http.MethodGet {
				getRequests++
			}
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(writer, `<Error><Code>AccessDenied</Code><Message>checksum permission denied</Message></Error>`)
			return
		}
		writer.Header().Set("ETag", `"object-etag"`)
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		if request.Method == http.MethodGet {
			_, _ = writer.Write(payload)
		}
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	store.checksumSHA256 = true
	defer store.Close()
	entry, err := store.Head(context.Background(), "test/checksum-fallback")
	if err != nil || entry.Version != `"object-etag"` || entry.ChecksumAlgorithm != "" {
		t.Fatalf("Head entry=%+v err=%v, want identity without unavailable checksum evidence", entry, err)
	}
	object, err := store.Get(context.Background(), "test/checksum-fallback")
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("Get payload=%q read=%v close=%v", actual, readErr, closeErr)
	}
	if headRequests != 1 || getRequests != 1 {
		t.Fatalf("checksum-mode denied requests: Head=%d Get=%d, want one fallback each", headRequests, getRequests)
	}
}
