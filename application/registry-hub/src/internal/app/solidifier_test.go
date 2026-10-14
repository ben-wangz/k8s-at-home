package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyLocalManifestRequestsSupportedMediaTypes(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	digest := artifact.CalculateSHA256(data)
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json") {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"OCI manifest not accepted"}]}`)
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		writer.Header().Set("Docker-Content-Digest", digest)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(data)
	}))
	defer registryServer.Close()

	endpoint, err := url.Parse(registryServer.URL)
	if err != nil {
		t.Fatalf("parse test Registry URL: %v", err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatalf("create Distribution client: %v", err)
	}
	defer registry.Close()

	solidification := &solidifier{registry: registry}
	path := "/v2/local/repository/manifests/" + digest
	if err := solidification.verifyLocalObject(context.Background(), path, digest, int64(len(data))); err != nil {
		t.Fatalf("verify local manifest: %v", err)
	}
}

func TestSolidifierGraphRejectsCyclesConflictsAndLimitsPermanently(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	processor := &solidifier{cfg: config.Config{Limits: config.Limits{ClosureDepth: 10}}}
	for _, test := range []struct {
		name  string
		class string
		run   func() error
	}{
		{
			name: "active DFS cycle", class: "manifest_cycle",
			run: func() error {
				graph := &closure{seen: make(map[string]contentRef), active: map[string]bool{digest: true}}
				_, err := processor.visitManifest(context.Background(), nil, lease.Lease{}, graph,
					contentRef{digest: digest, size: 1}, 1, false)
				return err
			},
		},
		{
			name: "descriptor size conflict", class: "manifest_invalid",
			run: func() error {
				graph := &closure{seen: map[string]contentRef{digest: {digest: digest, size: 10, mediaType: ociManifestMediaType}}, active: make(map[string]bool)}
				_, err := processor.visitManifest(context.Background(), nil, lease.Lease{}, graph,
					contentRef{digest: digest, size: 11, mediaType: ociManifestMediaType}, 1, false)
				return err
			},
		},
		{
			name: "recursive media type conflict", class: "manifest_invalid",
			run: func() error {
				graph := &closure{seen: map[string]contentRef{digest: {digest: digest, size: 10, mediaType: ociManifestMediaType}}, active: make(map[string]bool)}
				_, err := processor.visitManifest(context.Background(), nil, lease.Lease{}, graph,
					contentRef{digest: digest, size: 10, mediaType: dockerManifestMediaType}, 1, false)
				return err
			},
		},
		{
			name: "closure resource limit", class: "closure_limit",
			run: func() error {
				graph := &closure{seen: make(map[string]contentRef), active: make(map[string]bool)}
				return graph.addObject(contentRef{digest: digest, size: 9}, config.Limits{ClosureObjects: 1, ClosureBytes: 8})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			var failure *tasks.Failure
			if !errors.As(err, &failure) || !failure.Permanent || failure.Class != test.class {
				t.Fatalf("failure = %#v, want permanent class %q", err, test.class)
			}
		})
	}
}

func TestPermanentSolidificationGraphFailureDoesNotPublishRootOrRetry(t *testing.T) {
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
	childSize := int64(len(childBody))
	descriptors := []artifact.Descriptor{
		{MediaType: ociManifestMediaType, Digest: childDigest, Size: childSize},
		{MediaType: ociManifestMediaType, Digest: childDigest, Size: childSize + 1},
	}
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

	var registryRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		registryRequests.Add(1)
		writer.WriteHeader(http.StatusNotFound)
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

	cfg := config.Config{Limits: config.Limits{
		RequestTimeout: 2 * time.Second, ManifestBytes: 1 << 20, ObjectBytes: 1 << 20,
		MaxDescriptors: 10, DescriptorDataBytes: 1 << 20, ClosureDepth: 10,
		ClosureObjects: 100, ClosureBytes: 1 << 20,
	}}
	service := &Service{solidificationSlots: make(chan struct{}, 1), metrics: newMetrics()}
	processor := &solidifier{service: service, cfg: cfg, cache: cache, store: store, registry: registry}
	manager, err := tasks.NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	processor.tasks = manager
	service.tasks = manager
	task, _, err := manager.Enqueue(ctx, authority, repository, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDiscovered(ctx, &task, rootDigest, ociIndexMediaType); err != nil {
		t.Fatal(err)
	}
	manager.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := manager.Get(ctx, task.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == tasks.Failed {
			if current.Retryable || current.RetryCount != 0 || !current.NextAttemptAt.IsZero() || current.ErrorClass != "manifest_invalid" {
				t.Fatalf("terminal task state = %#v", current)
			}
			if registryRequests.Load() != 0 {
				t.Fatalf("permanent graph error issued %d Distribution requests", registryRequests.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := manager.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("solidification task did not reach permanent failure: %#v", current)
}
