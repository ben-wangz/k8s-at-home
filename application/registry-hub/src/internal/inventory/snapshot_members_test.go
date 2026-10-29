package inventory

import (
	"context"
	"errors"
	"net/url"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func assembleTestSnapshot(t *testing.T, writer *Writer) {
	t.Helper()
	for n := 0; n < 1000; n++ {
		done, err := writer.Assemble(context.Background(), PageSize)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
	t.Fatal("snapshot assembly did not converge")
}

func TestSnapshotPublicationRequiresReadyPlatformMembers(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "partial"}[partial], func(t *testing.T) {
			ctx := context.Background()
			repo := testRepository(t)
			previous := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
			assembleTestSnapshot(t, previous)
			if err := previous.Publish(ctx, true, nil); err != nil {
				t.Fatal(err)
			}
			writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
			identity := Identity{Authority: "example.org", Repository: "image", Reference: "latest"}
			childID := ID("platform", identity.Authority, identity.Repository, "child")
			image := Image{ID: identity.ID(), Scope: "reference", Identity: identity, Category: "unknown"}
			if err := writer.AddImage(ctx, image, nil, []string{childID}); err != nil {
				t.Fatal(err)
			}
			// A loose entry is not a completed member and must never satisfy a link.
			if err := repo.Immutable(ctx, writer.key("entries/"+childID+".json"), Image{ID: childID, Scope: "platform"}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Assemble(ctx, 1); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("dangling link assembly: %v", err)
			}
			if err := writer.Publish(ctx, !partial, nil); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("dangling link publication: %v", err)
			}
			current, err := repo.Snapshot(ctx, "", time.Minute)
			if err != nil || current.SnapshotID != previous.Snapshot.SnapshotID {
				t.Fatalf("previous snapshot lost: %+v %v", current, err)
			}
		})
	}
}

func TestSnapshotMembersAreCapturedAndCountedOnce(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	identity := Identity{Authority: "example.org", Repository: "image", Reference: "latest"}
	ref := ObjectRef{Digest: "root", Role: "manifest"}
	if err := writer.RequirePlatform(ctx, identity, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Assemble(ctx, 1); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("pending dependency: %v", err)
	}
	platform := Image{ID: ID("platform", identity.Authority, identity.Repository, ref.Digest), Scope: "platform", Identity: identity, Category: "unknown"}
	if err := writer.AddImage(ctx, platform, nil, nil); err != nil {
		t.Fatal(err)
	}
	changed := platform
	changed.Category = "solidified"
	if err := writer.AddImage(ctx, changed, []Object{{ID: "unexpected"}}, []string{"unexpected"}); err != nil {
		t.Fatal(err)
	}
	_, keys, _, err := writer.PendingPlatforms(ctx, "", 1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("pending=%v err=%v", keys, err)
	}
	if err := writer.CompletePlatform(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	image := Image{ID: identity.ID(), Scope: "reference", Identity: identity, ResolvedDigest: ref.Digest, Category: "unknown"}
	if err := writer.AddImage(ctx, image, nil, nil); err != nil {
		t.Fatal(err)
	}
	assembleTestSnapshot(t, writer)
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	page, err := repo.Images(ctx, Query{Kind: "images", Scope: "platform", Limit: 100, Filters: url.Values{}}, nil, time.Minute)
	if err != nil || len(page.Items) != 1 || page.Items[0].Category != "unknown" || page.UniqueManifests != 1 || page.Counts["platform:unknown"] != 1 || page.Counts["platform:solidified"] != 0 {
		t.Fatalf("captured membership=%+v err=%v", page, err)
	}
	if err := writer.AddImage(ctx, Image{ID: ID("late"), Scope: "platform"}, nil, nil); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("capture continued after seal: %v", err)
	}
	if err := writer.RequirePlatform(ctx, identity, ObjectRef{Digest: "late"}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("late dependency: %v", err)
	}
	if _, _, err := Read[[]Object](ctx, repo.Store, writer.key("objects/"+platform.ID+"/00000000.json")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retry overwrote captured payload: %v", err)
	}
}
