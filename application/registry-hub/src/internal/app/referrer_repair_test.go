package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestReferrerRepairReadMigratesPreviousSchema(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	now := time.Now().UTC()
	intent := referrerRepairIntent{
		SchemaVersion: referrerRepairSchemaPrevious,
		Authority:     "ghcr.io",
		Repository:    "team/image",
		Subject:       artifact.CalculateSHA256([]byte("subject")),
		Referrer:      artifact.CalculateSHA256([]byte("referrer")),
		Operation:     "add",
		Retryable:     true,
		Generation:    3,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	key := referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer)
	if _, err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}

	loaded, err := service.loadReferrerRepair(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != referrerRepairSchemaCurrent || loaded.Generation != intent.Generation || loaded.Operation != intent.Operation {
		t.Fatalf("migrated referrer repair intent = %#v, want preserved state at current schema", loaded)
	}
	object, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var persisted referrerRepairIntent
	if err := json.NewDecoder(object.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	if persisted.SchemaVersion != referrerRepairSchemaCurrent || persisted.Generation != intent.Generation {
		t.Fatalf("persisted repair intent after migration = %#v", persisted)
	}
}

func TestReferrerRepairRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	now := time.Now().UTC()
	intent := referrerRepairIntent{
		SchemaVersion: referrerRepairSchemaCurrent,
		Authority:     "ghcr.io",
		Repository:    "team/image",
		Subject:       artifact.CalculateSHA256([]byte("subject")),
		Referrer:      artifact.CalculateSHA256([]byte("referrer")),
		Operation:     "add",
		Retryable:     true,
		Generation:    3,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	key := referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer)
	if _, err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	unsupported := setAppTestSchemaVersion(t, store, key, 99)
	if _, err := service.loadReferrerRepair(ctx, key); err == nil {
		t.Fatal("referrer repair record with an unsupported schema was accepted")
	}
	after, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != unsupported.Version {
		t.Fatal("referrer repair record with an unsupported schema was rewritten")
	}
}

func TestSubjectPushKeepsManifestAndRecordsRepairAfterFallbackWriteFailure(t *testing.T) {
	ctx := context.Background()
	subjectBody := []byte("subject-manifest")
	subject := artifact.CalculateSHA256(subjectBody)
	emptyConfig := []byte("{}")
	manifestBody, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        &artifact.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: artifact.CalculateSHA256(emptyConfig), Size: int64(len(emptyConfig))},
		Layers:        []artifact.Descriptor{},
		Subject:       &artifact.Descriptor{MediaType: ociManifestMediaType, Digest: subject, Size: int64(len(subjectBody))},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := artifact.CalculateSHA256(manifestBody)
	fallbackTag := referrerFallbackTag(subject)
	manifestStored := false
	fallbackPuts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/referrers/"):
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/tags/list"):
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"name":"ghcr.io/team/image","tags":[]}`)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag) && request.Method == http.MethodGet:
			writeManifestMiss(writer)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+fallbackTag) && request.Method == http.MethodPut:
			fallbackPuts++
			writer.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(request.URL.Path, "/manifests/"+digest) && request.Method == http.MethodPut:
			stored, readErr := io.ReadAll(request.Body)
			if readErr != nil || !bytes.Equal(stored, manifestBody) {
				http.Error(writer, "manifest body mismatch", http.StatusBadRequest)
				return
			}
			manifestStored = true
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(request.URL.Path, "/manifests/latest") && request.Method == http.MethodGet:
			if !manifestStored {
				writeManifestMiss(writer)
				return
			}
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusOK)
		case strings.HasSuffix(request.URL.Path, "/manifests/latest") && request.Method == http.MethodPut:
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service, manager, registry, store, routed := newReferrerRepairTestService(t, server)
	defer manager.Close()
	defer registry.Close()
	defer store.Close()
	service.pushSlots = make(chan struct{}, 1)
	routed.Reference = digest
	routed.Endpoint = route.EndpointManifest
	query := url.Values{"tag": []string{"latest"}}
	request := httptest.NewRequest(http.MethodPut, "/v2/ghcr.io/team/image/manifests/"+digest, bytes.NewReader(manifestBody))
	request.Header.Set("Content-Type", ociManifestMediaType)
	recorder := httptest.NewRecorder()
	service.pushManifest(recorder, request, routed, query, &requestMeta{})

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("subject push response = %d Retry-After=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if !manifestStored || fallbackPuts != 1 {
		t.Fatalf("manifest stored=%t fallback PUTs=%d, want stored manifest and one failed fallback update", manifestStored, fallbackPuts)
	}
	intentKey := referrerRepairKey("ghcr.io", "team/image", subject, digest)
	intent, err := service.loadReferrerRepair(ctx, intentKey)
	if err != nil {
		t.Fatalf("load pending referrer repair: %v", err)
	}
	if !intent.Retryable || intent.Attempts != 1 || intent.Operation != "add" {
		t.Fatalf("pending referrer repair = %#v, want retryable add at attempt 1", intent)
	}
}
