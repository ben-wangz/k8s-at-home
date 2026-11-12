package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

type inspectionGuard struct {
	storage.Store
	source       storage.Fingerprinter
	payloadReads int
	writes       int
}

func (s *inspectionGuard) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasSuffix(key, "/data") {
		s.payloadReads++
	}
	return s.Store.Get(ctx, key)
}
func (s *inspectionGuard) Put(ctx context.Context, key string, r io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	s.writes++
	return s.Store.Put(ctx, key, r, size, opts)
}
func (s *inspectionGuard) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	return s.source.Fingerprint(ctx, key)
}

func TestCacheInspectionChecksVersionWithoutPayloadReadOrPin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache := NewCache(store)
	cache.EnableInventoryTracking()
	body := []byte("payload")
	digest := CalculateSHA256(body)
	media := "application/octet-stream"
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, media, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	guard := &inspectionGuard{Store: store, source: store}
	cache = NewCache(guard)
	inspection, err := cache.InspectAny(ctx, "ghcr.io", "team/image", digest, media)
	if err != nil || !inspection.Present || !inspection.InventoryTracked || guard.payloadReads != 0 || guard.writes != 0 {
		t.Fatalf("inspection=%+v reads=%d writes=%d err=%v", inspection, guard.payloadReads, guard.writes, err)
	}
	path := filepath.Join(root, "cache/objects/"+cacheIdentity("ghcr.io", "team/image", digest)+"/data")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	inspection, err = cache.Inspect(ctx, "ghcr.io", "team/image", digest, media)
	if err != nil || inspection.Present || inspection.Reason != "cache_version_mismatch" {
		t.Fatalf("changed=%+v err=%v", inspection, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	inspection, err = cache.Inspect(ctx, "ghcr.io", "team/image", digest, media)
	if err != nil || inspection.Present || inspection.Reason != "cache_payload_missing" {
		t.Fatalf("missing=%+v err=%v", inspection, err)
	}
}

func TestCacheInspectionLegacyFingerprintIsUnknown(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache := NewCache(store)
	body := []byte("legacy")
	digest := CalculateSHA256(body)
	media := "application/octet-stream"
	if err := cache.Put(ctx, "ghcr.io", "repo", digest, media, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	key := cacheMetadataKey(cacheIdentity("ghcr.io", "repo", digest), media)
	metadata, version, err := cache.readMetadataEntry(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	metadata.DataFingerprint = ""
	data, _ := json.Marshal(metadata)
	if _, err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); err != nil {
		t.Fatal(err)
	}
	inspection, err := cache.Inspect(ctx, "ghcr.io", "repo", digest, media)
	if err != nil || inspection.Present || !inspection.Unknown {
		t.Fatalf("legacy=%+v err=%v", inspection, err)
	}
}

func TestCacheInspectionAcceptsUnavailableBackendChecksum(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache := NewCache(store)
	body := []byte("backend checksum is optional")
	digest := CalculateSHA256(body)
	media := "application/octet-stream"
	if err := cache.Put(ctx, "ghcr.io", "repo", digest, media, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	identity := cacheIdentity("ghcr.io", "repo", digest)
	metadataKey := cacheMetadataKey(identity, media)
	metadata, version, err := cache.readMetadataEntry(ctx, metadataKey)
	if err != nil {
		t.Fatal(err)
	}
	metadata.IntegrityBackendAlgorithm = "sha256"
	metadata.IntegrityBackendValue = "backend-value"
	metadata.IntegrityBackendScope = "complete_object"
	data, _ := json.Marshal(metadata)
	if _, err := store.Put(ctx, metadataKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); err != nil {
		t.Fatal(err)
	}
	markerKey := cacheCompleteKey(identity, media)
	marker, err := cache.readComplete(ctx, identity, media)
	if err != nil {
		t.Fatal(err)
	}
	marker.IntegrityBackendAlgorithm = metadata.IntegrityBackendAlgorithm
	marker.IntegrityBackendValue = metadata.IntegrityBackendValue
	marker.IntegrityBackendScope = metadata.IntegrityBackendScope
	markerData, _ := json.Marshal(marker)
	markerEntry, err := store.Head(ctx, markerKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, markerKey, bytes.NewReader(markerData), int64(len(markerData)), storage.PutOptions{IfMatch: markerEntry.Version}); err != nil {
		t.Fatal(err)
	}
	inspection, err := cache.Inspect(ctx, "ghcr.io", "repo", digest, media)
	if err != nil || !inspection.Present {
		t.Fatalf("inspection=%+v err=%v, want optional backend checksum to tolerate a backend without read evidence", inspection, err)
	}
}
