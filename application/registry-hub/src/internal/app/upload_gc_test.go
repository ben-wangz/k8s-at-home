package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestUploadProxyKeepsWriteGateUntilSessionStateAndLeaseAreReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	baseStore, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &blockingUploadStore{Store: baseStore, entered: make(chan struct{}), proceed: make(chan struct{})}
	defer store.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			writer.Header().Set("Location", "/v2/repo/blobs/uploads/mock")
			writer.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Location", "/v2/repo/blobs/uploads/mock")
			writer.WriteHeader(http.StatusAccepted)
		case http.MethodDelete:
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	registry := newTestDistribution(t, server)
	leases := lease.NewManager(store)
	uploads := newUploadSessions(store, registry, leases, time.Minute, time.Hour)
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	sessionURL, err := uploads.Create(ctx, "repo", "repo", server.URL+"/v2/repo/blobs/uploads/mock")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(sessionURL)
	if err != nil {
		t.Fatal(err)
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, sessionURL, strings.NewReader("chunk"))
	if err != nil {
		t.Fatal(err)
	}
	store.blockNextUploadSave()
	proxyResult := make(chan error, 1)
	go func() {
		response, proxyErr := uploads.Proxy(request, "repo", path.Base(parsed.Path), query, gate, nil)
		if proxyErr != nil {
			proxyResult <- proxyErr
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			proxyResult <- fmt.Errorf("upload status = %d, want 202", response.StatusCode)
			return
		}
		proxyResult <- nil
	}()

	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("upload session persistence did not reach the blocked write")
	}
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatal(err)
	}
	quiesced := make(chan error, 1)
	go func() { quiesced <- gate.WaitQuiesced(ctx, time.Second) }()
	select {
	case err := <-quiesced:
		t.Fatalf("write gate quiesced before upload session persistence completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	store.allowBlockedWrite()
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("upload proxy did not finish")
	}
	select {
	case err := <-quiesced:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("write gate did not quiesce after upload persistence completed")
	}
	if gate.State().State != "backend_quiesced" {
		t.Fatalf("gate state = %q, want backend_quiesced", gate.State().State)
	}
	if err := uploads.AbortAll(ctx); err != nil {
		t.Fatalf("abort upload after quiescing: %v", err)
	}
}

type blockingUploadStore struct {
	storage.Store
	mu      sync.Mutex
	block   bool
	entered chan struct{}
	proceed chan struct{}
}

func (s *blockingUploadStore) blockNextUploadSave() {
	s.mu.Lock()
	s.block = true
	s.mu.Unlock()
}

func (s *blockingUploadStore) allowBlockedWrite() {
	close(s.proceed)
}

func (s *blockingUploadStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.mu.Lock()
	block := s.block && strings.HasPrefix(key, "uploads/sessions/")
	if block {
		s.block = false
	}
	s.mu.Unlock()
	if block {
		close(s.entered)
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return storage.Entry{}, ctx.Err()
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}
