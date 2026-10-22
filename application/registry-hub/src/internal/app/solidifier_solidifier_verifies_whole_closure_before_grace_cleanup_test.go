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
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSolidifierPublishesClosureAfterIndependentBlobCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache := artifact.NewCache(store)
	authority, repository := "ghcr.io", "team/image"
	blobBody := []byte("abc")
	blobDigest := artifact.CalculateSHA256(blobBody)
	configDescriptor := artifact.Descriptor{
		MediaType: "application/vnd.oci.empty.v1+json", Digest: blobDigest, Size: int64(len(blobBody)),
		Data: json.RawMessage(`"YWJj"`),
	}
	childBody, err := json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configDescriptor, Layers: []artifact.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	childDigest := artifact.CalculateSHA256(childBody)
	descriptors := []artifact.Descriptor{{MediaType: ociManifestMediaType, Digest: childDigest, Size: int64(len(childBody))}}
	rootBody, err := json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &descriptors})
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := artifact.CalculateSHA256(rootBody)
	for _, object := range []struct {
		digest, mediaType string
		body              []byte
	}{{rootDigest, ociIndexMediaType, rootBody}, {childDigest, ociManifestMediaType, childBody}} {
		if err := cache.Put(ctx, authority, repository, object.digest, object.mediaType, bytes.NewReader(object.body), int64(len(object.body))); err != nil {
			t.Fatal(err)
		}
	}

	type registryObject struct {
		body      []byte
		mediaType string
		digest    string
	}
	var objectMu sync.Mutex
	objects := make(map[string]registryObject)
	var verifiedReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/blobs/uploads/") && request.Method == http.MethodPost {
			writer.Header().Set("Location", request.URL.Path+"acceptance-session")
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.Contains(request.URL.Path, "/blobs/uploads/") && request.Method == http.MethodPut {
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "invalid body", http.StatusBadRequest)
				return
			}
			digest := request.URL.Query().Get("digest")
			if artifact.CalculateSHA256(body) != digest {
				http.Error(writer, "digest mismatch", http.StatusBadRequest)
				return
			}
			object := registryObject{body: body, mediaType: "application/octet-stream", digest: digest}
			objectMu.Lock()
			objects[digest] = object
			objectMu.Unlock()
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
			return
		}
		if index := strings.LastIndex(request.URL.Path, "/blobs/"); index >= 0 {
			reference := request.URL.Path[index+len("/blobs/"):]
			objectMu.Lock()
			object, exists := objects[reference]
			objectMu.Unlock()
			if !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
			writer.Header().Set("Docker-Content-Digest", object.digest)
			if request.Method == http.MethodGet {
				verifiedReads.Add(1)
				_, _ = writer.Write(object.body)
			} else {
				writer.WriteHeader(http.StatusOK)
			}
			return
		}
		manifestPath := "/manifests/"
		index := strings.LastIndex(request.URL.Path, manifestPath)
		if index < 0 {
			http.NotFound(writer, request)
			return
		}
		reference := request.URL.Path[index+len(manifestPath):]
		switch request.Method {
		case http.MethodGet:
			objectMu.Lock()
			object, exists := objects[reference]
			objectMu.Unlock()
			if !exists {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
				return
			}
			verifiedReads.Add(1)
			writer.Header().Set("Content-Type", object.mediaType)
			writer.Header().Set("Docker-Content-Digest", object.digest)
			_, _ = writer.Write(object.body)
		case http.MethodPut:
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "invalid body", http.StatusBadRequest)
				return
			}
			digest := artifact.CalculateSHA256(body)
			object := registryObject{body: body, mediaType: request.Header.Get("Content-Type"), digest: digest}
			objectMu.Lock()
			objects[reference] = object
			objectMu.Unlock()
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.Header().Set("Location", request.URL.Path)
			writer.WriteHeader(http.StatusCreated)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Limits: config.Limits{
		RequestTimeout: 2 * time.Second, ManifestBytes: 1 << 20, ObjectBytes: 1 << 20,
		MaxDescriptors: 10, DescriptorDataBytes: 1 << 20, ClosureDepth: 10,
		ClosureObjects: 100, ClosureBytes: 1 << 20,
	}}
	service := &Service{cfg: cfg, store: store, gate: gate, records: tasks.NewRecords(store), solidificationSlots: make(chan struct{}, 1), metrics: newMetrics()}
	processor := &solidifier{service: service, cfg: cfg, cache: cache, store: store, registry: registry, gate: gate}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	service.tasks = manager
	processor.tasks = manager
	task, _, err := manager.Enqueue(ctx, authority, repository, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDiscovered(ctx, &task, rootDigest, ociIndexMediaType); err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	task.Status = tasks.Running
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	rootErr := processor.Process(ctx, &task, taskLease)
	var rootYield *tasks.Yield
	if !errors.As(rootErr, &rootYield) || rootYield.Status != tasks.WaitingDeps {
		t.Fatalf("first root processing result = %v, want waiting_dependencies", rootErr)
	}
	if _, exists := objects["latest"]; exists {
		t.Fatal("root reference was published before its blob dependency was available in Distribution")
	}
	task.Status = rootYield.Status
	task.WaitReason = rootYield.Reason
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	intent, err := service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: authority, Repository: repository, Digest: blobDigest})
	if err != nil {
		t.Fatal(err)
	}
	blobTask, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	blobLease, err := manager.Leases().Acquire(ctx, "tasks/"+blobTask.ID, "blob-test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, blobLease)
	blobTask.Status = tasks.Running
	if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
		t.Fatal(err)
	}
	blobErr := processor.Process(ctx, &blobTask, blobLease)
	var blobYield *tasks.Yield
	if !errors.As(blobErr, &blobYield) || blobYield.Status != tasks.VerifiedLocal {
		t.Fatalf("blob processing result = %v, want verified_local", blobErr)
	}
	blobTask, err = manager.Get(ctx, blobTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	blobTask.Status = blobYield.Status
	blobTask.WaitReason = blobYield.Reason
	if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
		t.Fatal(err)
	}
	processor.TaskYielded(ctx, &blobTask)
	removed, err := cache.Cleanup(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup after verified blob removed=%d err=%v, want 1", removed, err)
	}
	retained, err := cache.GetAnyRetained(ctx, authority, repository, blobDigest)
	if err == nil {
		_ = retained.Body.Close()
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("verified blob cache lookup error = %v, want not found after cleanup", err)
	}
	if err := storeDataObjectCount(t, ctx, store, 2); err != nil {
		t.Fatal(err)
	}
	task, err = manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = tasks.Running
	task.WaitReason = ""
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := processor.Process(ctx, &task, taskLease); err != nil {
		t.Fatalf("resume root after blob verification: %v (%#v)", err, err)
	}
	if task.Checkpoints[rootDigest] != "verified" || task.Checkpoints[childDigest] != "verified" {
		t.Fatalf("solidification checkpoints = %#v", task.Checkpoints)
	}
	if verifiedReads.Load() < 4 {
		t.Fatalf("Distribution verification reads = %d, want blob, child, root digest, and root tag", verifiedReads.Load())
	}
	objectMu.Lock()
	rootByTag, tagExists := objects["latest"]
	objectMu.Unlock()
	if !tagExists || rootByTag.digest != rootDigest || !bytes.Equal(rootByTag.body, rootBody) {
		t.Fatalf("root tag was not published with the verified root: %#v", rootByTag)
	}

	removed, err = cache.Cleanup(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || removed != 3 {
		t.Fatalf("cleanup after root anchor removed=%d err=%v, want 3", removed, err)
	}
	if err := storeDataObjectCount(t, ctx, store, 0); err != nil {
		t.Fatal(err)
	}
}

func storeDataObjectCount(t *testing.T, ctx context.Context, store storage.Store, want int) error {
	t.Helper()
	cacheEntries, _, err := store.List(ctx, "cache/objects/", "", 100)
	if err != nil {
		return err
	}
	dataEntries := 0
	dataKeys := make([]string, 0)
	for _, entry := range cacheEntries {
		if strings.HasSuffix(entry.Key, "/data") {
			dataEntries++
			dataKeys = append(dataKeys, entry.Key)
		}
	}
	if dataEntries != want {
		return fmt.Errorf("cache data objects = %d, want %d: %v", dataEntries, want, dataKeys)
	}
	return nil
}
