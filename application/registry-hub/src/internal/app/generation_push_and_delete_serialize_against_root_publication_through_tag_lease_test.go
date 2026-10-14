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
	"registry-hub/internal/config"
	"registry-hub/internal/route"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPushAndDeleteSerializeAgainstRootPublicationThroughTagLease(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	rootBody := []byte("root manifest")
	rootDigest := artifact.CalculateSHA256(rootBody)
	var mu sync.Mutex
	tagPresent := false
	getCount := 0
	putCount := 0
	deleteCount := 0
	firstGetStarted := make(chan struct{})
	releaseFirstGet := make(chan struct{})
	var releaseFirstGetOnce sync.Once
	releaseFirst := func() { releaseFirstGetOnce.Do(func() { close(releaseFirstGet) }) }
	server := newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			mu.Lock()
			getCount++
			currentGet := getCount
			present := tagPresent
			mu.Unlock()
			if currentGet == 1 {
				close(firstGetStarted)
				<-releaseFirstGet
			}
			if !present {
				writeManifestMiss(writer)
				return
			}
			writer.Header().Set("Content-Type", ociManifestMediaType)
			writer.Header().Set("Content-Length", strconv.Itoa(len(rootBody)))
			writer.Header().Set("Docker-Content-Digest", rootDigest)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(rootBody)
		case http.MethodPut:
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil || !bytes.Equal(body, rootBody) {
				http.Error(writer, "unexpected root body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			tagPresent = true
			putCount++
			mu.Unlock()
			writer.Header().Set("Docker-Content-Digest", rootDigest)
			writer.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			mu.Lock()
			tagPresent = false
			deleteCount++
			mu.Unlock()
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(releaseFirst)
	cache := artifact.NewCache(store)
	service := &Service{
		cfg: config.Config{Limits: config.Limits{
			ManifestBytes: 1 << 20, MaxDescriptors: 10, DescriptorDataBytes: 1 << 20,
		}},
		store: store, tasks: manager, cache: cache, gate: gate,
		metrics: newMetrics(), pushSlots: make(chan struct{}, 1), registry: server,
	}
	const authority, repository, reference = "ghcr.io", "team/image", "latest"
	initialGeneration, err := service.advanceGeneration(ctx, authority, repository, reference, false)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.Enqueue(ctx, authority, repository, reference)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDiscovered(ctx, &task, rootDigest, ociManifestMediaType, initialGeneration.Generation); err != nil {
		t.Fatal(err)
	}
	task.Graph[rootDigest] = tasks.GraphEntry{Digest: rootDigest, MediaType: ociManifestMediaType, Size: int64(len(rootBody))}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, authority, repository, rootDigest, ociManifestMediaType, bytes.NewReader(rootBody), int64(len(rootBody))); err != nil {
		t.Fatal(err)
	}
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	processor := &solidifier{service: service, cfg: service.cfg, tasks: manager, cache: cache, store: store, registry: server, gate: gate}
	publishResult := make(chan error, 1)
	go func() { publishResult <- processor.publishRootTag(ctx, &task, taskLease) }()
	select {
	case <-firstGetStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("root publication did not reach the held tag read")
	}
	pushBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifestMediaType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    artifact.CalculateSHA256([]byte("{}")),
			"size":      2,
		},
		"layers": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	pushRequest := httptest.NewRequest(http.MethodPut, routed.RawPath, bytes.NewReader(pushBody))
	pushRequest.Header.Set("Content-Type", ociManifestMediaType)
	pushResponse := httptest.NewRecorder()
	service.pushManifest(pushResponse, pushRequest, routed, make(url.Values), &requestMeta{})
	if pushResponse.Code != http.StatusConflict {
		t.Fatalf("push during root publication = %d, want 409: %s", pushResponse.Code, pushResponse.Body.String())
	}
	mu.Lock()
	registryCallsDuringPush := getCount + putCount + deleteCount
	mu.Unlock()
	if registryCallsDuringPush != 1 {
		t.Fatalf("Registry calls while push lost the tag lease = %d, want only the held solidification GET", registryCallsDuringPush)
	}
	unchanged, err := service.generation(ctx, authority, repository, reference)
	if err != nil || unchanged.Generation != initialGeneration.Generation {
		t.Fatalf("generation changed during rejected push: %#v err=%v", unchanged, err)
	}

	firstDelete := httptest.NewRecorder()
	service.deleteLocal(firstDelete, httptest.NewRequest(http.MethodDelete, routed.RawPath, nil), routed, make(url.Values), &requestMeta{})
	if firstDelete.Code != http.StatusConflict {
		t.Fatalf("delete during root publication = %d, want 409", firstDelete.Code)
	}
	unchanged, err = service.generation(ctx, authority, repository, reference)
	if err != nil || unchanged.Generation != initialGeneration.Generation {
		t.Fatalf("generation changed during rejected delete: %#v err=%v", unchanged, err)
	}

	releaseFirst()
	select {
	case err := <-publishResult:
		if err != nil {
			t.Fatalf("publish root tag: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root publication did not finish")
	}
	published, err := manager.Get(ctx, task.ID)
	if err != nil || !published.RootPublished || published.RootPublishing {
		t.Fatalf("task after root publication = %#v err=%v", published, err)
	}

	secondDelete := httptest.NewRecorder()
	service.deleteLocal(secondDelete, httptest.NewRequest(http.MethodDelete, routed.RawPath, nil), routed, make(url.Values), &requestMeta{})
	if secondDelete.Code != http.StatusAccepted {
		t.Fatalf("delete after root publication = %d, want 202: %s", secondDelete.Code, secondDelete.Body.String())
	}
	deletedGeneration, err := service.generation(ctx, authority, repository, reference)
	if err != nil || deletedGeneration.Generation != initialGeneration.Generation+1 {
		t.Fatalf("generation after successful delete = %#v err=%v", deletedGeneration, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if putCount != 1 || deleteCount != 1 || tagPresent {
		t.Fatalf("Registry operations PUT=%d DELETE=%d tagPresent=%t, want one serialized publish and delete", putCount, deleteCount, tagPresent)
	}
}
