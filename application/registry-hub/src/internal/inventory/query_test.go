package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"registry-hub/internal/storage"
	"sort"
	"testing"
	"time"
)

func TestSnapshotSparsePagingAndIsolation(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	buildStarted := time.Now()
	for n := 0; n < 1007; n++ {
		category := "waiting_cache"
		if n%113 == 0 {
			category = "solidified"
		}
		image := Image{ID: ID("item", fmt.Sprint(n)), Scope: "reference", Category: category, Identity: Identity{Authority: "ghcr.io", Repository: "example", Reference: fmt.Sprint(n)}}
		if err := writer.AddImage(ctx, image, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	assembleTestSnapshot(t, writer)
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem snapshot: images=1007 build=%s", time.Since(buildStarted))
	counter := &inventoryReadCounter{Store: repo.Store}
	repo.Store = counter
	latencies := make([]time.Duration, 20)
	for n := range latencies {
		started := time.Now()
		page, err := repo.Images(ctx, Query{Kind: "images", Scope: "reference", Limit: 100, Filters: url.Values{}}, []byte("key"), time.Minute)
		if err != nil || len(page.Items) != 100 {
			t.Fatalf("warm list: %v", err)
		}
		latencies[n] = time.Since(started)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("filesystem warm list: limit=100 samples=20 p50=%s p95=%s store_gets_per_request=%d", latencies[10], latencies[18], counter.reads/20)
	if counter.reads != 40 {
		t.Fatalf("warm page reads=%d want=40", counter.reads)
	}
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatalf("publication retry: %v", err)
	}
	q := Query{Kind: "images", Scope: "reference", Limit: 2, Filters: url.Values{"category": {"solidified"}}}
	key := []byte("cursor-secret")
	page, err := repo.Images(ctx, q, key, time.Minute)
	if err != nil || len(page.Items) != 2 || page.NextToken == "" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	q.Token = page.NextToken
	newer := repo.NewWriter(time.Now().UTC(), time.Hour, 2)
	assembleTestSnapshot(t, newer)
	if err := newer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range page.Items {
		seen[item.ID] = true
	}
	for q.Token != "" {
		page, err = repo.Images(ctx, q, key, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if page.SnapshotID != writer.Snapshot.SnapshotID {
			t.Fatal("pagination switched snapshot")
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate item")
			}
			seen[item.ID] = true
		}
		q.Token = page.NextToken
	}
	if len(seen) != 9 {
		t.Fatalf("sparse results=%d want=9", len(seen))
	}
	q.Token = mustCursor(t, key, cursor{SnapshotID: writer.Snapshot.SnapshotID, Query: q.fingerprint(), ExpiresAt: time.Now().Add(-time.Second)})
	if _, err := repo.Images(ctx, q, key, time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token=%v", err)
	}
	q.Token = mustCursor(t, key, cursor{SnapshotID: writer.Snapshot.SnapshotID, Query: q.fingerprint(), ExpiresAt: time.Now().Add(time.Minute)})
	q.Filters.Set("category", "failed")
	if _, err := repo.Images(ctx, q, key, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("filter reuse=%v", err)
	}
	if err := writer.Publish(ctx, true, nil); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old builder replaced current: %v", err)
	}
}

type inventoryReadCounter struct {
	storage.Store
	reads int
}

func (s *inventoryReadCounter) Get(ctx context.Context, key string) (storage.Object, error) {
	s.reads++
	return s.Store.Get(ctx, key)
}

func mustCursor(t *testing.T, key []byte, c cursor) string {
	t.Helper()
	value, err := token(key, c)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSlowBuildRetainsSnapshotAfterPublication(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().Add(-2*time.Hour).UTC(), time.Hour, 1)
	assembleTestSnapshot(t, writer)
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.Snapshot(ctx, "", time.Minute)
	if err != nil || snapshot.Freshness.State != "stale" || time.Until(snapshot.ExpiresAt) < 59*time.Minute {
		t.Fatalf("slow snapshot=%+v err=%v", snapshot, err)
	}
	expiry := snapshot.ExpiresAt
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if !writer.Snapshot.ExpiresAt.Equal(expiry) {
		t.Fatal("publication retry extended immutable snapshot retention")
	}
}
