package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type closureRegistryObject struct {
	body      []byte
	mediaType string
	digest    string
}

type closurePublicationFixture struct {
	ctx           context.Context
	store         storage.Store
	cache         *artifact.Cache
	authority     string
	repository    string
	blobBody      []byte
	blobDigest    string
	childBody     []byte
	childDigest   string
	rootBody      []byte
	rootDigest    string
	objectMu      sync.Mutex
	objects       map[string]closureRegistryObject
	verifiedReads atomic.Int32
	service       *Service
	processor     *solidifier
	manager       *tasks.Manager
	task          tasks.Task
	taskLease     lease.Lease
	blobTask      tasks.Task
	blobLease     lease.Lease
}

func TestSolidifierPublishesClosureAfterIndependentBlobCleanup(t *testing.T) {
	fixture := newClosurePublicationFixture(t)
	fixture.startRootTask(t)
	fixture.waitForBlobDependency(t)
	fixture.processBlobAndCleanup(t)
	fixture.resumeRootAndVerify(t)
}

func newClosurePublicationFixture(t *testing.T) *closurePublicationFixture {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fixture := &closurePublicationFixture{
		ctx: context.Background(), store: store, cache: artifact.NewCache(store),
		authority: "ghcr.io", repository: "team/image", blobBody: []byte("abc"),
		objects: make(map[string]closureRegistryObject),
	}
	fixture.prepareImage(t)
	server := httptest.NewServer(http.HandlerFunc(fixture.serveDistribution))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	gate, err := newWriteGate(fixture.ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Limits: config.Limits{
		RequestTimeout: 2 * time.Second, ManifestBytes: 1 << 20, ObjectBytes: 1 << 20,
		MaxDescriptors: 10, DescriptorDataBytes: 1 << 20, ClosureDepth: 10,
		ClosureObjects: 100, ClosureBytes: 1 << 20,
	}}
	fixture.service = &Service{cfg: cfg, store: store, gate: gate, records: tasks.NewRecords(store), solidificationSlots: make(chan struct{}, 1), metrics: newMetrics()}
	fixture.processor = &solidifier{service: fixture.service, cfg: cfg, cache: fixture.cache, store: store, registry: registry, gate: gate}
	fixture.manager, err = tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fixture.manager.Close)
	fixture.service.tasks = fixture.manager
	fixture.processor.tasks = fixture.manager
	return fixture
}

func (fixture *closurePublicationFixture) prepareImage(t *testing.T) {
	t.Helper()
	fixture.blobDigest = artifact.CalculateSHA256(fixture.blobBody)
	configDescriptor := artifact.Descriptor{
		MediaType: "application/vnd.oci.empty.v1+json", Digest: fixture.blobDigest, Size: int64(len(fixture.blobBody)),
		Data: json.RawMessage(`"YWJj"`),
	}
	var err error
	fixture.childBody, err = json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configDescriptor, Layers: []artifact.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.childDigest = artifact.CalculateSHA256(fixture.childBody)
	descriptors := []artifact.Descriptor{{MediaType: ociManifestMediaType, Digest: fixture.childDigest, Size: int64(len(fixture.childBody))}}
	fixture.rootBody, err = json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &descriptors})
	if err != nil {
		t.Fatal(err)
	}
	fixture.rootDigest = artifact.CalculateSHA256(fixture.rootBody)
	for _, object := range []struct {
		digest, mediaType string
		body              []byte
	}{{fixture.rootDigest, ociIndexMediaType, fixture.rootBody}, {fixture.childDigest, ociManifestMediaType, fixture.childBody}} {
		if err := fixture.cache.Put(fixture.ctx, fixture.authority, fixture.repository, object.digest, object.mediaType, bytes.NewReader(object.body), int64(len(object.body))); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *closurePublicationFixture) serveDistribution(writer http.ResponseWriter, request *http.Request) {
	if strings.HasSuffix(request.URL.Path, "/blobs/uploads/") && request.Method == http.MethodPost {
		writer.Header().Set("Location", request.URL.Path+"acceptance-session")
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if strings.Contains(request.URL.Path, "/blobs/uploads/") && request.Method == http.MethodPut {
		fixture.acceptBlobUpload(writer, request)
		return
	}
	if index := strings.LastIndex(request.URL.Path, "/blobs/"); index >= 0 {
		fixture.serveBlob(writer, request, request.URL.Path[index+len("/blobs/"):])
		return
	}
	index := strings.LastIndex(request.URL.Path, "/manifests/")
	if index < 0 {
		http.NotFound(writer, request)
		return
	}
	fixture.serveManifest(writer, request, request.URL.Path[index+len("/manifests/"):])
}

func (fixture *closurePublicationFixture) acceptBlobUpload(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "invalid body", http.StatusBadRequest)
		return
	}
	digest := request.URL.Query().Get("digest")
	if artifact.CalculateSHA256(body) != digest {
		http.Error(writer, "digest mismatch", http.StatusBadRequest)
		return
	}
	fixture.objectMu.Lock()
	fixture.objects[digest] = closureRegistryObject{body: body, mediaType: "application/octet-stream", digest: digest}
	fixture.objectMu.Unlock()
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.WriteHeader(http.StatusCreated)
}

func (fixture *closurePublicationFixture) serveBlob(writer http.ResponseWriter, request *http.Request, digest string) {
	fixture.objectMu.Lock()
	object, exists := fixture.objects[digest]
	fixture.objectMu.Unlock()
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
	writer.Header().Set("Docker-Content-Digest", object.digest)
	if request.Method == http.MethodGet {
		fixture.verifiedReads.Add(1)
		_, _ = writer.Write(object.body)
		return
	}
	writer.WriteHeader(http.StatusOK)
}
