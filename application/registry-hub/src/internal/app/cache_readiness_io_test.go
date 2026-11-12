package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

type readinessPayloadStore struct {
	*filesystem.Store
	opens int
}

func (s *readinessPayloadStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	s.opens++
	return s.Store.OpenPayload(ctx, key)
}

func TestReadinessUsesNoPayloadAndLegacyVerificationIsDurable(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &readinessPayloadStore{Store: base}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	blob := f.cacheObject([]byte("candidate payload"), "application/octet-stream")
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest}
	if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if store.opens != 0 {
		t.Fatalf("readiness opened %d payload handles", store.opens)
	}
	entries, _, err := storage.Scan(ctx, f.service.store, "cache/objects/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Key, "/representations/") {
			err := inventory.Update(ctx, f.service.store, entry.Key, func(metadata *map[string]any) error { (*metadata)["dataFingerprint"] = ""; return nil })
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	inspection, err := f.service.inspectReadyBlob(ctx, f.service.cache, identity.Authority, identity.Repository, identity.Digest)
	if err != nil || !inspection.Unknown || inspection.Present || store.opens != 0 {
		t.Fatalf("unknown readiness=%+v, opens=%d, error=%v", inspection, store.opens, err)
	}
	jobs, _, err := storage.Scan(ctx, f.service.store, "background/v1/jobs/cache_verification/", "", 100)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("verification jobs=%v, %v", jobs, err)
	}
	queue := f.service.backgroundQueue()
	id := strings.TrimSuffix(jobs[0].Key[strings.LastIndex(jobs[0].Key, "/")+1:], ".json")
	job, err := queue.Read(ctx, "cache_verification", id)
	if err != nil {
		t.Fatal(err)
	}
	job, err = queue.ClaimFor(ctx, job, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, done, err := f.service.cacheVerificationStep(ctx, &job)
	if err != nil || !done || store.opens != 1 {
		t.Fatalf("verification done=%t opens=%d error=%v", done, store.opens, err)
	}
	inspection, err = f.service.cache.InspectAny(ctx, identity.Authority, identity.Repository, identity.Digest, blob.MediaType)
	if err != nil || !inspection.Present {
		t.Fatalf("legacy verification did not refresh proof: %+v, %v", inspection, err)
	}
	pins, _, err := storage.Scan(ctx, f.service.store, "cache/readers/", "", 100)
	if err != nil || len(pins) != 0 {
		t.Fatalf("verification leaked pins: %v, %v", pins, err)
	}
}
