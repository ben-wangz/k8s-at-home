package s3store

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventorySnapshotDependenciesThroughS3Adapter(t *testing.T) {
	fixture := &inventoryS3Fixture{objects: map[string][]byte{}}
	server := httptest.NewServer(fixture)
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 64<<20, 5<<20)
	defer store.Close()
	repo := &inventory.Repository{Store: store}
	ctx := context.Background()
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	identity := inventory.Identity{Authority: "example.org", Repository: "image", Reference: "latest"}
	ref := inventory.ObjectRef{Role: "manifest", Digest: "manifest", Platform: &inventory.Platform{OS: "linux", Architecture: "amd64"}}
	if err := writer.RequirePlatform(ctx, identity, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AssembleStep(ctx, 1); !errors.Is(err, inventory.ErrIncomplete) {
		t.Fatalf("pending dependency accepted: %v", err)
	}
	requests, keys, _, err := writer.PendingPlatforms(ctx, "", 1)
	if err != nil || len(requests) != 1 || requests[0].Platform == nil {
		t.Fatalf("pending dependencies=%+v err=%v", requests, err)
	}
	platformID := inventory.ID("platform", identity.Authority, identity.Repository, ref.Digest)
	platform := inventory.Image{ID: platformID, Scope: "platform", Identity: identity, Category: "unknown", Platform: ref.Platform}
	// Exercise unknown-commit readback during capture as well as during assembly.
	repo.Store = &inventoryUnknownCommitStore{Store: store}
	if err := writer.AddImage(ctx, platform, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.CompletePlatform(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	image := inventory.Image{ID: identity.ID(), Scope: "reference", Identity: identity, ResolvedDigest: ref.Digest, Category: "unknown"}
	if err := writer.AddImage(ctx, image, nil, []string{platformID}); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddCacheObject(ctx, inventory.Object{ID: inventory.ID("object"), CacheState: "complete"}); err != nil {
		t.Fatal(err)
	}
	repo.Store = &inventoryUnknownCommitStore{Store: store}
	for n := 0; ; n++ {
		done, err := writer.AssembleStep(ctx, 1)
		if err != nil || n > 100 {
			t.Fatalf("assembly: done=%v err=%v", done, err)
		}
		if done {
			break
		}
	}
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	query := inventory.Query{Kind: "platforms", ImageID: image.ID, Limit: 1, Filters: url.Values{}}
	page, err := repo.Images(ctx, query, nil, time.Minute)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != platformID || page.UniqueManifests != 1 || page.Counts["platform:unknown"] != 1 {
		t.Fatalf("platforms=%+v err=%v", page, err)
	}
	query = inventory.Query{Kind: "images", Scope: "reference", Limit: 1, Filters: url.Values{"platform": {"linux/amd64"}}}
	page, err = repo.Images(ctx, query, nil, time.Minute)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("platform filter=%+v err=%v", page, err)
	}
	objects, err := repo.Objects(ctx, inventory.Query{Kind: "cache", Limit: 100}, nil, time.Minute)
	if err != nil || len(objects.Items) != 1 {
		t.Fatalf("cache objects=%+v err=%v", objects, err)
	}
}
