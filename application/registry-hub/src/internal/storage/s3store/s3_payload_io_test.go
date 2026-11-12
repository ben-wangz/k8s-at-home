package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"sync/atomic"
	"testing"
	"time"
)

func TestS3ConditionalConsumptionChecksETagBeforeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"verified"` {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("ETag", `"verified"`)
		_, _ = io.WriteString(w, "payload")
	}))
	defer server.Close()
	store := newProductionRetryTestStore(t, server.URL)
	object, err := store.GetIfMatch(context.Background(), "layer", `"verified"`)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil || object.Version != `"verified"` || string(body) != "payload" {
		t.Fatalf("conditional GET=%+v body=%q error=%v", object, body, err)
	}
	if _, err := store.GetIfMatch(context.Background(), "layer", `"replaced"`); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("changed ETag error=%v", err)
	}
}

func TestS3PayloadCountersIncludeRetriedWireBodies(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		w.Header().Set("ETag", `"committed"`)
	}))
	defer server.Close()
	store := newProductionRetryTestStore(t, server.URL)
	var wireBytes atomic.Int64
	ctx := storage.WithIOObserver(context.Background(), func(_ context.Context, operation string, count int64, _ time.Duration) {
		if operation == "s3_write" {
			wireBytes.Add(count)
		}
	})
	payload := bytes.Repeat([]byte("retry body"), 4096)
	if _, err := store.Put(ctx, "layer", bytes.NewReader(payload), int64(len(payload)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || wireBytes.Load() != int64(2*len(payload)) {
		t.Fatalf("attempts=%d wire bytes=%d, want two complete bodies (%d)", attempts.Load(), wireBytes.Load(), 2*len(payload))
	}
}
