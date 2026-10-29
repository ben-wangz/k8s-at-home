package inventory

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestCurrentExpiryDoesNotExtendPinnedPagination(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	expired := Snapshot{SchemaVersion: 1, SnapshotID: ID("expired"), ObservedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Second)}
	for _, key := range []string{Prefix + "current.json", Prefix + "snapshots/" + expired.SnapshotID + "/manifest.json"} {
		if err := repo.Immutable(ctx, key, expired); err != nil {
			t.Fatal(err)
		}
	}
	q := Query{Kind: "images", Scope: "reference", Limit: 1, Filters: url.Values{}}
	if _, err := repo.Images(ctx, q, []byte("key"), time.Minute); !errors.Is(err, ErrCurrentExpired) {
		t.Fatalf("expired latest query=%v", err)
	}
	q.Token = mustCursor(t, []byte("key"), cursor{SnapshotID: expired.SnapshotID, Query: q.fingerprint(), ExpiresAt: time.Now().Add(time.Minute)})
	if _, err := repo.Images(ctx, q, []byte("key"), time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatalf("pinned cursor outlived its snapshot: %v", err)
	}
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	assembleTestSnapshot(t, writer)
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Images(ctx, q, []byte("key"), time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatalf("new publication revived an expired cursor: %v", err)
	}
	q.Token = ""
	if _, err := repo.Images(ctx, q, []byte("key"), time.Minute); err != nil {
		t.Fatalf("new publication did not restore latest queries: %v", err)
	}
}
