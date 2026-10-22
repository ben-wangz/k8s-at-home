package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

type stagedAcceptanceObject struct {
	body      []byte
	mediaType string
}

type stagedAcceptanceFixture struct {
	t         *testing.T
	service   *Service
	processor *solidifier
	mu        sync.Mutex
	objects   map[string]stagedAcceptanceObject
	uploads   map[string]int
	requests  int
}

func newStagedAcceptanceFixture(t *testing.T, maxBytes int64) *stagedAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	cfg := config.Config{
		Limits: config.Limits{RequestTimeout: 30 * time.Second, BlobTransferTimeout: 30 * time.Second,
			ManifestBytes: 1 << 20, ObjectBytes: 1 << 20, MaxDescriptors: 100,
			DescriptorDataBytes: 1 << 20, ClosureDepth: 10, ClosureObjects: 100, ClosureBytes: 1 << 20},
		Solidification: config.Solidification{MaxProtectionSessions: 10,
			MaxProtectedCacheBytes: 1 << 20, MaxUnanchoredDistributionBytes: maxBytes},
	}
	cache := artifact.NewCache(store)
	f := &stagedAcceptanceFixture{t: t, objects: make(map[string]stagedAcceptanceObject), uploads: make(map[string]int)}
	registry := newLocalRegistry(t, http.HandlerFunc(f.serveHTTP))
	f.service = &Service{cfg: cfg, store: store, cache: cache, registry: registry, gate: gate,
		records: tasks.NewRecords(store), tasks: manager, metrics: newMetrics(), solidificationSlots: make(chan struct{}, 1)}
	f.processor = &solidifier{service: f.service, cfg: cfg, cache: cache, store: store,
		registry: registry, gate: gate, tasks: manager}
	return f
}

func (f *stagedAcceptanceFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if strings.HasSuffix(r.URL.Path, "/blobs/uploads/") && r.Method == http.MethodPost {
		w.Header().Set("Location", r.URL.Path+"acceptance")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if strings.Contains(r.URL.Path, "/blobs/uploads/") && r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		digest := r.URL.Query().Get("digest")
		if err != nil || artifact.CalculateSHA256(body) != digest {
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
		f.uploads[digest]++
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}
	index := strings.LastIndex(r.URL.Path, "/manifests/")
	manifest := index >= 0
	if !manifest {
		index = strings.LastIndex(r.URL.Path, "/blobs/")
	}
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	reference := r.URL.Path[index+len("/blobs/"):]
	if manifest {
		reference = r.URL.Path[index+len("/manifests/"):]
	}
	if manifest && r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid manifest", http.StatusBadRequest)
			return
		}
		f.objects[reference] = stagedAcceptanceObject{body: body, mediaType: r.Header.Get("Content-Type")}
		w.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(body))
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		return
	}
	object, ok := f.objects[reference]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
		}
		return
	}
	w.Header().Set("Content-Type", object.mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
	w.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(object.body))
	if r.Method != http.MethodHead {
		_, _ = w.Write(object.body)
	}
}

func (f *stagedAcceptanceFixture) cacheObject(body []byte, mediaType string) artifact.Descriptor {
	f.t.Helper()
	descriptor := artifact.Descriptor{Digest: artifact.CalculateSHA256(body), Size: int64(len(body)), MediaType: mediaType}
	if err := f.service.cache.Put(context.Background(), "ghcr.io", "team/image", descriptor.Digest,
		mediaType, bytes.NewReader(body), descriptor.Size); err != nil {
		f.t.Fatal(err)
	}
	return descriptor
}

func (f *stagedAcceptanceFixture) cacheManifest(manifest artifact.Manifest) artifact.Descriptor {
	f.t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.cacheObject(body, manifest.MediaType)
}

func (f *stagedAcceptanceFixture) root(reference string, descriptor artifact.Descriptor) tasks.Task {
	f.t.Helper()
	task, _, err := f.service.tasks.EnqueueResolved(context.Background(), "ghcr.io", "team/image",
		reference, descriptor.Digest, descriptor.MediaType, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}

func (f *stagedAcceptanceFixture) step(id string) error {
	f.t.Helper()
	ctx := context.Background()
	task, err := f.service.tasks.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	currentLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+id, "staged-acceptance", time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, currentLease)
	task.Status, task.WaitReason = tasks.Running, ""
	if err := f.service.tasks.SaveProgress(ctx, &task, currentLease); err != nil {
		f.t.Fatal(err)
	}
	result := f.processor.Process(ctx, &task, currentLease)
	task, err = f.service.tasks.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	var yielded *tasks.Yield
	switch {
	case errors.As(result, &yielded):
		task.Status, task.WaitReason = yielded.Status, yielded.Reason
	case result == nil:
		task.Status = tasks.Solidified
	default:
		task.Status = tasks.Failed
		var failure *tasks.Failure
		if errors.As(result, &failure) && !failure.Permanent {
			task.Status = tasks.RetryWait
		}
	}
	if err := f.service.tasks.SaveProgress(ctx, &task, currentLease); err != nil {
		f.t.Fatal(err)
	}
	f.processor.TaskYielded(ctx, &task)
	return result
}

func (f *stagedAcceptanceFixture) transfer(descriptor artifact.Descriptor) {
	f.t.Helper()
	ctx := context.Background()
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: descriptor.Digest,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.step(intent.ID)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.VerifiedLocal {
		f.t.Fatalf("transfer %s = %v, want verified_local", descriptor.Digest, err)
	}
}

func (f *stagedAcceptanceFixture) published(reference string) (stagedAcceptanceObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[reference]
	return object, ok
}

func TestStagedPlatformsPublishIndependentlyWithDelayedArm64Demand(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	amd := f.cacheObject([]byte("amd64 configuration"), "application/octet-stream")
	amd.MediaType = "application/vnd.oci.image.config.v1+json"
	armBody := []byte("arm64 configuration")
	arm := artifact.Descriptor{Digest: artifact.CalculateSHA256(armBody), Size: int64(len(armBody)), MediaType: amd.MediaType}
	amdManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &amd, Layers: []artifact.Descriptor{}})
	armManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &arm, Layers: []artifact.Descriptor{}})
	amdManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"amd64"}`)
	armManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"arm64"}`)
	children := []artifact.Descriptor{amdManifest, armManifest}
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("latest", rootDescriptor)
	wantWaiting := func(err error) {
		t.Helper()
		var yielded *tasks.Yield
		if !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
			t.Fatalf("root processing = %v, want waiting_dependencies", err)
		}
	}
	wantWaiting(f.step(root.ID))
	f.transfer(amd)
	wantWaiting(f.step(root.ID))
	if _, ok := f.published(amdManifest.Digest); !ok {
		t.Fatal("ready amd64 platform was not published before arm64 arrived")
	}
	for _, reference := range []string{armManifest.Digest, rootDescriptor.Digest, "latest"} {
		if _, ok := f.published(reference); ok {
			t.Fatalf("incomplete scope %s was published", reference)
		}
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("anchored amd64 and cold arm64 still block GC: %#v", state)
	}
	if len(f.service.solidificationSlots) != 0 {
		t.Fatal("dependency wait retained a worker slot")
	}
	wantWaiting(f.step(root.ID))
	f.cacheObject(armBody, "application/octet-stream")
	wantWaiting(f.step(root.ID))
	f.transfer(arm)
	if err := f.step(root.ID); err != nil {
		t.Fatalf("finish root after arm64 arrives: %v", err)
	}
	object, ok := f.published("latest")
	if !ok || artifact.CalculateSHA256(object.body) != rootDescriptor.Digest {
		t.Fatal("completed root does not preserve the original index bytes")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads[amd.Digest] != 1 || f.uploads[arm.Digest] != 1 {
		t.Fatalf("platform blob uploads = %v, want one for each platform", f.uploads)
	}
}

func TestOversizedStagedScopeDoesNotContactDistributionOrCleanCache(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 512)
	configDescriptor := f.cacheObject(bytes.Repeat([]byte("c"), 128), "application/octet-stream")
	layers := []artifact.Descriptor{f.cacheObject(bytes.Repeat([]byte("l"), 256), "application/octet-stream")}
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &configDescriptor, Layers: layers})
	root := f.root("oversized", rootDescriptor)
	err := f.step(root.ID)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "capacity_limit" {
		t.Fatalf("oversized scope result = %v, want permanent capacity_limit", err)
	}
	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	if requests != 0 {
		t.Fatalf("oversized scope made %d Distribution calls", requests)
	}
	if removed, err := f.service.cache.Cleanup(context.Background(), time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
		t.Fatalf("cleanup after capacity rejection = %d, %v, want retained inputs", removed, err)
	}
	for _, descriptor := range append(layers, configDescriptor) {
		cached, err := f.service.cache.GetAnyRetained(context.Background(), root.Authority, root.Repository, descriptor.Digest)
		if err != nil {
			t.Fatalf("rejected scope lost cache input: %v", err)
		}
		_ = cached.Body.Close()
	}
	if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("oversized scope acquired protection or quota: %#v", state)
	}
}

func TestWrongDescriptorSizeDoesNotPoisonSharedBlobForAnotherRoot(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	shared := f.cacheObject([]byte("shared configuration"), "application/octet-stream")
	shared.MediaType = "application/vnd.oci.image.config.v1+json"
	wrong := shared
	wrong.Size++
	bad := f.root("bad-size", f.cacheManifest(artifact.Manifest{SchemaVersion: 2,
		MediaType: ociManifestMediaType, Config: &wrong, Layers: []artifact.Descriptor{}}))
	err := f.step(bad.ID)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "manifest_invalid" {
		t.Fatalf("wrong edge result = %v, want permanent manifest_invalid", err)
	}
	goodDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2,
		MediaType: ociManifestMediaType, Config: &shared, Layers: []artifact.Descriptor{}})
	good := f.root("correct-size", goodDescriptor)
	var yielded *tasks.Yield
	if err := f.step(good.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("valid root with shared bytes = %v, want waiting for independent transfer", err)
	}
	f.transfer(shared)
	if err := f.step(good.ID); err != nil {
		t.Fatalf("valid root after unrelated size failure: %v", err)
	}
	if _, ok := f.published("bad-size"); ok {
		t.Fatal("invalid root was published")
	}
	if object, ok := f.published("correct-size"); !ok || artifact.CalculateSHA256(object.body) != goodDescriptor.Digest {
		t.Fatal("unrelated wrong-size edge poisoned the valid root")
	}
}

func TestReusedLocalBlobAndMissingLayerDoNotCompleteAbandonedPull(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	localBody := []byte("configuration already in Distribution")
	local := artifact.Descriptor{Digest: artifact.CalculateSHA256(localBody), Size: int64(len(localBody)),
		MediaType: "application/vnd.oci.image.config.v1+json"}
	f.objects[local.Digest] = stagedAcceptanceObject{body: localBody, mediaType: "application/octet-stream"}
	missing := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("client never requested this layer")),
		Size: 33, MediaType: "application/vnd.oci.image.layer.v1.tar"}
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &local, Layers: []artifact.Descriptor{missing}})
	root := f.root("abandoned-pull", descriptor)
	for range 2 {
		var yielded *tasks.Yield
		if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
			t.Fatalf("incomplete client pull = %v, want dependency wait", err)
		}
	}
	for _, reference := range []string{descriptor.Digest, "abandoned-pull"} {
		if _, ok := f.published(reference); ok {
			t.Fatalf("incomplete manifest %s was published", reference)
		}
	}
	if len(f.service.solidificationSlots) != 0 || len(f.uploads) != 0 {
		t.Fatal("unissued layer request caused background payload work or retained a worker")
	}
	current, err := f.service.tasks.Get(context.Background(), root.ID)
	if err != nil || current.Status != tasks.WaitingDeps {
		t.Fatalf("abandoned root = %#v, %v", current, err)
	}
}

func TestRootlessVerifiedBlobProtectionCanBeCancelledOrExpired(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("expire=%t", expire), func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("direct blob without a manifest"), "application/octet-stream")
			intent, err := f.service.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
				Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest,
			})
			if err != nil {
				t.Fatal(err)
			}
			cached, err := f.service.cache.GetAnyRetained(ctx, intent.Identity.Authority, intent.Identity.Repository, blob.Digest)
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.activateBlobIntent(ctx, intent, cached)
			_ = cached.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.transfer(blob)
			if state := f.service.gate.State(); state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != blob.Size {
				t.Fatalf("rootless verified blob protection = %#v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC before rootless termination = %v", err)
			}
			if expire {
				expired, err := f.service.tasks.CancelIfExpired(ctx, intent.ID, time.Now().Add(25*time.Hour))
				if err != nil || !expired {
					t.Fatalf("expire rootless verified task = %t, %v", expired, err)
				}
			} else if _, err := f.service.tasks.Cancel(ctx, intent.ID); err != nil {
				t.Fatal(err)
			}
			task, err := f.service.tasks.Get(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.processor.TaskTerminal(ctx, &task); err != nil {
				t.Fatal(err)
			}
			if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
				t.Fatalf("rootless safe termination retained protection = %#v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); err != nil {
				t.Fatalf("GC after rootless safe termination: %v", err)
			}
		})
	}
}

func TestClientReaderSurvivesBlobTransferAndCleanupBeforeManifestPublication(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	body := []byte("client response remains open while background transfer finishes")
	blob := f.cacheObject(body, "application/octet-stream")
	reader, err := f.service.cache.GetAnyRetained(ctx, "ghcr.io", "team/image", blob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Body.Close()
	prefix := make([]byte, 7)
	if _, err := io.ReadFull(reader.Body, prefix); err != nil {
		t.Fatal(err)
	}
	blob.MediaType = "application/vnd.oci.image.config.v1+json"
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root("pinned-reader", descriptor)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("prepare root = %v", err)
	}
	f.transfer(blob)
	if _, err := f.service.records.FindLocalObservation(ctx, root.Authority, root.Repository, tasks.GraphBlob,
		blob.Digest, blob.Size, 0, f.service.gate.State().Epoch); err != nil {
		t.Fatalf("durable verification before cleanup: %v", err)
	}
	if _, ok := f.published(descriptor.Digest); ok {
		t.Fatal("manifest published before the explicit finalization step")
	}
	if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
		t.Fatalf("cleanup with active client reader = %d, %v", removed, err)
	}
	rest, err := io.ReadAll(reader.Body)
	if err != nil || !bytes.Equal(append(prefix, rest...), body) {
		t.Fatalf("client response after background transfer = %q, %v", rest, err)
	}
	if err := reader.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 1 {
		t.Fatalf("cleanup after reader close but before manifest publication = %d, %v", removed, err)
	}
	if f.service.gate.State().ProtectionSessionCount != 1 {
		t.Fatal("blob cache was removed without continuous unanchored protection")
	}
	if err := f.step(root.ID); err != nil {
		t.Fatalf("publish manifest after independent blob cleanup: %v", err)
	}
}
