package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"strings"
	"testing"
	"time"
)

func TestInventoryRetainedPlatformUsesLatestTerminalTaskOutcome(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	identity := inventoryIdentity("example.org", "image", "latest")
	digest := artifact.CalculateSHA256([]byte("retained"))
	id := strings.Repeat("a", 32)
	old := inventoryWork{Identity: identity, Digest: digest, UpdatedAt: time.Now().Add(-time.Hour), Work: inventory.Work{ID: id, Kind: "root", Status: "pending"}}
	latest := old
	latest.Work.Status, latest.UpdatedAt = "cancelled", time.Now().UTC()
	contentID := identity.ContentID(digest)
	if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"content-work/"+contentID+"/"+id+".json", func(value *inventoryWork) error { *value = old; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"work/"+identity.ID()+"/"+id+".json", func(value *inventoryWork) error { *value = latest; return nil }); err != nil {
		t.Fatal(err)
	}
	p := &inventoryImageProgress{Image: inventory.Image{ID: inventory.ID("platform", digest), Scope: "platform", Identity: identity, ContentID: contentID}}
	done, err := f.service.advanceInventoryImageWork(ctx, p)
	if err != nil || !done || len(p.Image.Solidification.Tasks) != 1 {
		t.Fatalf("retained task projection: done=%v image=%+v err=%v", done, p.Image, err)
	}
	work := p.Image.Solidification.Tasks[0]
	if work.Status != "cancelled" || !work.HistoryUnavailable {
		t.Fatalf("stale active status survived task cleanup: %+v", work)
	}
}
