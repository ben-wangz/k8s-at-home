package s3store

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGetBodyErrorAfterHeadersIsNotRetriedOrReportedAsComplete(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		gets.Add(1)
		writer.Header().Set("ETag", `"get-etag"`)
		writer.Header().Set("Content-Length", "12")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "short")
	}))
	defer server.Close()

	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	defer store.Close()
	object, err := store.Get(context.Background(), "test/truncated-get")
	if err != nil {
		t.Fatalf("Get response headers: %v", err)
	}
	data, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if string(data) != "short" || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated GET body = %q, read error = %v; want partial bytes plus unexpected EOF", data, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close truncated GET body: %v", closeErr)
	}
	if attempts := gets.Load(); attempts != 1 {
		t.Fatalf("GET attempts after response body failure = %d, want one", attempts)
	}
}
