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
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func collectTestStorage(t *testing.T, s *Service, registry storage.Store) storageSnapshot {
	t.Helper()
	state := newStorageCollection()
	for steps := 0; state.NextScanAt.IsZero(); steps++ {
		if steps > 20000 {
			t.Fatal("storage collection did not converge")
		}
		if err := s.advanceStorageCollection(context.Background(), &state, registry, nil); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		state = storageCollection{}
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.storageSnapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStorageInventoryPhysicalTotalsAndSnapshotPagination(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	registry, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for n := 0; n < 7; n++ {
		key := fmt.Sprintf("docker/registry/v2/blobs/sha256/ab/%064x/data", n)
		body := bytes.Repeat([]byte("r"), n+1)
		if _, err := registry.Put(ctx, key, bytes.NewReader(body), int64(len(body)), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := storage.Scan(ctx, f.service.store, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	expectedState := int64(0)
	for _, entry := range entries {
		expectedState += entry.Size
	}
	snapshot := collectTestStorage(t, f.service, inspectionReadOnlyStore{registry})
	if *snapshot.Domains["state"].ObjectBytes != expectedState || *snapshot.Domains["registry"].ObjectBytes != 28 {
		t.Fatalf("physical totals: %+v expected state=%d", snapshot.Domains, expectedState)
	}
	for _, domain := range snapshot.Domains {
		sum := int64(0)
		for _, category := range domain.Categories {
			sum += category.ObjectBytes
		}
		if sum != *domain.ObjectBytes {
			t.Fatal("categories double-count or omit objects")
		}
	}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store, f.service.inventory.Store = guard, guard
	path := "/api/v1/storage/objects?domain=registry&limit=2"
	seen := map[string]bool{}
	for pageNumber := 0; path != ""; pageNumber++ {
		w := diagnosticGet(t, f.service, path)
		var page storageObjectPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("page: %d %s", w.Code, w.Body.String())
		}
		if page.SnapshotID != snapshot.SnapshotID {
			t.Fatal("page silently changed snapshots")
		}
		for _, object := range page.Items {
			if seen[object.ID] {
				t.Fatal("duplicate object")
			}
			seen[object.ID] = true
			if diagnosticGet(t, f.service, "/api/v1/storage/objects/"+object.ID).Code != 200 {
				t.Fatal("detail lookup failed")
			}
		}
		if pageNumber == 0 {
			if err := inventory.Update(ctx, guard.Store, storageInventoryPrefix+"current.json", func(value *storageSnapshot) error { value.SnapshotID = inventory.ID("new-report"); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/storage/objects?domain=registry&limit=2&token=" + url.QueryEscape(page.NextToken)
		}
		if pageNumber > 1000 {
			t.Fatal("pagination failed to terminate")
		}
	}
	if len(seen) != 7 || guard.writes != 0 {
		t.Fatalf("seen=%d writes=%d", len(seen), guard.writes)
	}
	if diagnosticGet(t, f.service, "/api/v1/storage/usage?digest=ignored").Code != 400 {
		t.Fatal("ignored filter accepted")
	}
}

func TestS3RegistryInspectionCollectsMetadataAndRejectsWrites(t *testing.T) {
	const blobData = "blob"
	const blobDigestPath = "docker/registry/v2/blobs/sha256/ab/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/data"
	const repositoryLink = "docker/registry/v2/repositories/team/image/_manifests/tags/latest/current/link"
	var listRequests, objectReads, writeRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2" {
			listRequests.Add(1)
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>registry-inspection</Name><Prefix>distribution/</Prefix><KeyCount>2</KeyCount><MaxKeys>4</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>distribution/%s</Key><LastModified>2026-10-31T00:00:00Z</LastModified><ETag>"blob-etag"</ETag><Size>4</Size></Contents><Contents><Key>distribution/%s</Key><LastModified>2026-10-31T00:00:00Z</LastModified><ETag>"link-etag"</ETag><Size>4</Size></Contents></ListBucketResult>`, blobDigestPath, repositoryLink)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/registry-inspection/distribution/"+blobDigestPath {
			objectReads.Add(1)
			writer.Header().Set("ETag", `"blob-etag"`)
			writer.Header().Set("Last-Modified", "Sat, 31 Oct 2026 00:00:00 GMT")
			writer.Header().Set("Content-Length", fmt.Sprint(len(blobData)))
			_, _ = io.WriteString(writer, blobData)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writeRequests.Add(1)
		}
		http.Error(writer, "unexpected S3 request", http.StatusNotFound)
	}))
	defer server.Close()

	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.RegistryInspection = config.RegistryInspection{Backend: "s3", S3: config.S3{Endpoint: server.URL, Region: "us-east-1", Bucket: "registry-inspection", Prefix: "distribution", AccessKeyID: "inspection-access", SecretAccessKey: "inspection-secret", UsePathStyle: true, AllowHTTP: true}}
	registry, err := f.service.registryInspectionStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	entries, next, err := storage.Scan(context.Background(), registry, "", "", 4)
	if err != nil || next != "" || len(entries) != 2 || entries[0].Key != blobDigestPath || entries[1].Key != repositoryLink {
		t.Fatalf("registry inspection listing=%+v next=%q err=%v", entries, next, err)
	}
	object, err := registry.Get(context.Background(), blobDigestPath)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != blobData {
		t.Fatalf("registry inspection read=%q readErr=%v closeErr=%v", body, readErr, closeErr)
	}
	if _, err := registry.Put(context.Background(), "unexpected", strings.NewReader("x"), 1, storage.PutOptions{}); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("inspection Put error=%v", err)
	}
	if err := registry.Delete(context.Background(), blobDigestPath); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("inspection Delete error=%v", err)
	}
	if err := registry.DeleteIfMatch(context.Background(), blobDigestPath, "blob-etag"); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("inspection DeleteIfMatch error=%v", err)
	}
	snapshot := collectTestStorage(t, f.service, registry)
	if *snapshot.Domains["registry"].ObjectBytes != 8 || *snapshot.Domains["registry"].Objects != 2 || listRequests.Load() < 2 || objectReads.Load() != 1 || writeRequests.Load() != 0 {
		t.Fatalf("registry S3 inspection totals=%+v list=%d get=%d writes=%d", snapshot.Domains["registry"], listRequests.Load(), objectReads.Load(), writeRequests.Load())
	}
}

func TestStorageUsageCountsSharedPhysicalObjectOnce(t *testing.T) {
	state := newStorageCollection()
	shared := true
	object := storageObject{Domain: "registry", Category: "registry_payloads", ObjectBytes: 20, Shared: &shared, Associations: []storageAssociation{{ImageID: "a", CanonicalImageID: "one", Authority: "example.org", Repository: "repo"}, {ImageID: "b", CanonicalImageID: "two", Authority: "example.org", Repository: "repo"}}}
	addStorageUsage(&state, object)
	if *state.Snapshot.Domains["registry"].ObjectBytes != 20 {
		t.Fatal("physical object counted repeatedly")
	}
	repo := state.Usage["registry\x00repository\x00example.org/repo"]
	if repo.ObjectBytes != 20 || repo.SharedBytes == nil || *repo.SharedBytes != 20 {
		t.Fatalf("repository usage=%+v", repo)
	}
	for _, image := range []string{"a", "b"} {
		if state.Usage["registry\x00image\x00"+image].ObjectBytes != 20 {
			t.Fatal("missing logical owner")
		}
	}
}

func TestOrphanReservationIsQueryableWithoutTransfer(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	data := []byte(`{"reservations":{"private":{"identity":"private-source","digest":"sha256:abc","session":"orphan","bytes":42}}}`)
	if _, err := f.service.store.Put(context.Background(), "downloads/v1/quota.json", bytes.NewReader(data), int64(len(data)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	w := diagnosticGet(t, f.service, "/api/v1/download-reservations")
	var page diagnosticPage[reservationView]
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].StateEvidence != "state_absent" || page.Items[0].ReservedBytes != 42 {
		t.Fatalf("reservation: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-source") {
		t.Fatal("private key leaked")
	}
}

func TestStorageAttributionExpiredSourceDoesNotBlockPhysicalCollection(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	state := newStorageCollection()
	state.AttributionPhase, state.Snapshot.InventorySnapshotID = 1, inventory.ID("gone")
	if err := f.service.advanceStorageCollection(context.Background(), &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	if state.AttributionPhase != 3 || !strings.Contains(state.Snapshot.AttributionCoverage, "partial") {
		t.Fatal("expired attribution source blocks collection")
	}
	snapshot := storageSnapshot{SnapshotID: inventory.ID("temporary"), CompletedAt: time.Now(), TransferTemporaryBytes: map[string]int64{"transfer": 12}}
	view := transferView{ID: "transfer"}
	attachTransferStorage(&view, &snapshot)
	if view.TemporaryBytes == nil || *view.TemporaryBytes != 12 {
		t.Fatal("physical residuals omitted")
	}
}
