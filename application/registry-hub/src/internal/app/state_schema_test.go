package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestWriteGateRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := newWriteGate(ctx, store); err != nil {
		t.Fatal(err)
	}
	unsupported := setAppTestSchemaVersion(t, store, "system/gc/state.json", 99)
	if _, err := newWriteGate(ctx, store); err == nil {
		t.Fatal("write gate with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, "system/gc/state.json", unsupported.Version)
}

func TestGCRunRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	run := gcRun{SchemaVersion: 1, ID: strings.Repeat("a", 32), Mode: "kubernetes", State: "failed"}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	const key = "system/gc/last-run.json"
	if _, err := store.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	unsupported := setAppTestSchemaVersion(t, store, key, 99)
	if _, err := newGCController(&Service{store: store}); err == nil {
		t.Fatal("GC run with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, key, unsupported.Version)
}

func TestUploadSessionRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const id = "0123456789abcdef0123456789abcdef"
	const key = "uploads/sessions/" + id + ".json"
	data := []byte(`{"schemaVersion":99,"id":"` + id + `"}`)
	entry, err := store.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&uploadSessions{store: store}).readState(ctx, id); err == nil {
		t.Fatal("upload session with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, key, entry.Version)
}

func TestReferrerMigrationRejectsUnknownSchemaWithoutRewritingRecord(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	const authority, repository = "ghcr.io", "team/image"
	const subject = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := service.storeReferrerMigration(ctx, authority, repository, subject,
		"sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"sha256:3333333333333333333333333333333333333333333333333333333333333333", nil); err != nil {
		t.Fatal(err)
	}
	key := referrerMigrationKey(authority, repository, subject)
	unsupported := setAppTestSchemaVersion(t, store, key, 99)
	if _, err := service.loadReferrerMigration(ctx, authority, repository, subject); err == nil {
		t.Fatal("referrer migration with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, key, unsupported.Version)
}

func TestFallbackProtectionMarkersRejectUnknownSchemaWithoutRewritingRecords(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	routed, err := taskRoute("ghcr.io", "team/image")
	if err != nil {
		t.Fatal(err)
	}
	subject := artifact.CalculateSHA256([]byte("subject"))
	digest := artifact.CalculateSHA256([]byte("fallback-index"))
	marker := referrerFallbackMarker{
		SchemaVersion: 99,
		Authority:     "ghcr.io",
		Repository:    "team/image",
		Subject:       subject,
		Tag:           referrerFallbackTag(subject),
		Digest:        digest,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	tagKey := fallbackTagMarkerKey(routed, marker.Tag)
	tagEntry, err := store.Put(ctx, tagKey, strings.NewReader(string(data)), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.readMarker(ctx, tagKey, routed, marker.Tag); err == nil {
		t.Fatal("fallback tag marker with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, tagKey, tagEntry.Version)

	digestKey := fallbackDigestMarkerKey(routed, digest)
	digestData, err := json.Marshal([]referrerFallbackMarker{marker})
	if err != nil {
		t.Fatal(err)
	}
	digestEntry, err := store.Put(ctx, digestKey, strings.NewReader(string(digestData)), int64(len(digestData)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.readDigestMarkers(ctx, digestKey, routed, digest); err == nil {
		t.Fatal("fallback digest marker with an unsupported schema was accepted")
	}
	assertStoredVersionUnchanged(t, store, digestKey, digestEntry.Version)
}

func assertStoredVersionUnchanged(t *testing.T, store storage.Store, key, version string) {
	t.Helper()
	entry, err := store.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != version {
		t.Fatalf("record %q was rewritten: version = %q, want %q", key, entry.Version, version)
	}
}
