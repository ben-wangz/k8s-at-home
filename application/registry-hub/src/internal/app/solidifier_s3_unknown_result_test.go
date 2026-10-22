package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/s3store"
	"registry-hub/internal/tasks"
)

func TestS3UnknownBlobResultRetainsProtectionUntilReadbackAndAnchor(t *testing.T) {
	for key, value := range map[string]string{"AWS_ACCESS_KEY_ID": "test-key", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "", "AWS_EC2_METADATA_DISABLED": "true", "AWS_MAX_ATTEMPTS": "2", "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null"} {
		t.Setenv(key, value)
	}
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	blob := f.cacheObject([]byte("S3 result response lost after Distribution commits"), "application/octet-stream")
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root("s3-unknown-result", descriptor)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("prepare root = %v", err)
	}
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: blob.Digest})
	if err != nil {
		t.Fatal(err)
	}
	key := "solidification/blobs/intents/" + intent.ID + ".json"
	object, err := f.service.store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	remote := &unknownResultS3Server{body: body, version: `"initial"`}
	server := httptest.NewServer(remote)
	defer server.Close()
	s3, err := s3store.New(ctx, config.S3{Endpoint: server.URL, Region: "us-east-1", Bucket: "registry-hub", AllowHTTP: true,
		UsePathStyle: true, MultipartThreshold: 64 << 20, PartSize: 5 << 20, Timeout: 30 * time.Second, MaxConnections: 2, ReplayDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	f.service.records = tasks.NewRecords(&intentS3Store{Store: f.service.store, remote: s3, key: key})
	if err := f.step(intent.ID); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("unknown S3 result and failed readback = %v", err)
	}
	remote.mu.Lock()
	committed, attempts := remote.unknown, remote.verifiedAttempts
	remote.mu.Unlock()
	if !committed || attempts != 2 {
		t.Fatalf("S3 committed=%t verified PUT attempts=%d, want lost response then conditional conflict", committed, attempts)
	}
	state := f.service.gate.State()
	if state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes == 0 || state.ProtectedCacheBytes != blob.Size || len(f.service.solidificationSlots) != 0 {
		t.Fatalf("unknown S3 result lost inputs, quota, or worker release: %#v", state)
	}
	if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC after unknown S3 result = %v", err)
	}
	if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
		t.Fatalf("cleanup after unknown S3 result = %d, %v", removed, err)
	}
	remote.mu.Lock()
	remote.recovered = true
	remote.mu.Unlock()
	f.transfer(blob)
	if err := f.step(root.ID); err != nil {
		t.Fatalf("anchor after S3 readback recovery: %v", err)
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 || state.ProtectedCacheBytes != 0 {
		t.Fatalf("anchored S3 recovery retained quota: %#v", state)
	}
	f.mu.Lock()
	uploads := f.uploads[blob.Digest]
	f.mu.Unlock()
	if uploads != 1 {
		t.Fatalf("Distribution uploads across S3 recovery = %d, want 1", uploads)
	}
}

type intentS3Store struct {
	storage.Store
	remote storage.Store
	key    string
}

func (s *intentS3Store) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == s.key {
		return s.remote.Get(ctx, key)
	}
	return s.Store.Get(ctx, key)
}

func (s *intentS3Store) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if key == s.key {
		return s.remote.Put(ctx, key, body, size, options)
	}
	return s.Store.Put(ctx, key, body, size, options)
}

type unknownResultS3Server struct {
	mu               sync.Mutex
	body             []byte
	version          string
	operation        string
	unknown          bool
	recovered        bool
	verifiedAttempts int
}

func (s *unknownResultS3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(r.URL.Path, "/solidification/blobs/intents/") {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut {
		data, err := io.ReadAll(r.Body)
		var intent tasks.BlobIntent
		if err != nil || json.Unmarshal(data, &intent) != nil {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if intent.State == tasks.IntentVerified {
			s.verifiedAttempts++
		}
		if match := r.Header.Get("If-Match"); match != "" && match != s.version {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		s.body = bytes.Clone(data)
		s.version = `"` + artifact.CalculateSHA256(data) + `"`
		s.operation = r.Header.Get("X-Amz-Meta-Registry-Hub-Operation")
		if intent.State == tasks.IntentVerified && !s.unknown {
			s.unknown = true
			http.Error(w, "response lost after commit", http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", s.version)
		return
	}
	if s.unknown && !s.recovered {
		http.Error(w, "readback unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
	w.Header().Set("ETag", s.version)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("X-Amz-Meta-Registry-Hub-Operation", s.operation)
	if r.Method != http.MethodHead {
		_, _ = w.Write(s.body)
	}
}
