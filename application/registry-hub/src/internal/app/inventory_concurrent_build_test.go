package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryCapturesPlatformsArrivingAfterContentScan(t *testing.T) {
	for _, variant := range []string{"complete", "descriptors", "missing_child", "retag"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			f.service.cfg.Inventory = f.service.inventoryConfig()
			f.service.cfg.Inventory.BatchSize = 1
			identity := inventoryIdentity("ghcr.io", "team/image", "latest")
			if variant == "retag" {
				old := projectInventoryTestIndex(t, f.service, identity, nil)
				bindInventoryTestDigest(t, f.service, identity, old.Digest)
			}
			writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, 1)
			gate, err := f.service.inventoryEpoch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			writer.Snapshot.GCEpoch = gate.Epoch
			state := inventoryCheckpoint{Build: &writer.Snapshot, Scan: inventoryScan{Complete: true}}
			for state.BuildPhase == 0 {
				if err := f.service.buildInventoryPage(ctx, &state); err != nil {
					t.Fatal(err)
				}
			}
			children := []artifact.Descriptor{
				{Digest: artifact.CalculateSHA256([]byte("amd64")), MediaType: ociManifestMediaType, Size: 5, Platform: json.RawMessage(`{"os":"linux","architecture":"amd64"}`)},
				{Digest: artifact.CalculateSHA256([]byte("arm64")), MediaType: ociManifestMediaType, Size: 5, Platform: json.RawMessage(`{"os":"linux","architecture":"arm64"}`)},
			}
			descriptors := children
			if variant == "missing_child" {
				descriptors = append(append([]artifact.Descriptor{}, children...), children[0])
			}
			root := projectInventoryTestIndex(t, f.service, identity, descriptors)
			if variant == "complete" {
				for _, child := range children {
					content := inventory.Content{Identity: identity, ObjectRef: inventoryDescriptor(child, "manifest"), ClosureKnown: true, Source: "manifest", UpdatedAt: time.Now().UTC()}
					if err := f.service.inventory.SaveContent(ctx, content); err != nil {
						t.Fatal(err)
					}
				}
			}
			if variant == "missing_child" {
				if err := f.service.store.Delete(ctx, inventory.Prefix+"contents/"+identity.ContentID(children[0].Digest)+".json"); err != nil {
					t.Fatal(err)
				}
			}
			bindInventoryTestDigest(t, f.service, identity, root.Digest)
			for n := 0; state.Build != nil; n++ {
				if n > 100 {
					t.Fatal("late dependency build did not converge")
				}
				if err := f.service.buildInventoryPage(ctx, &state); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			parent := inventoryTestImage(t, f.service, snapshot, identity.ID())
			if parent.ResolvedDigest != root.Digest {
				t.Fatalf("late binding lost: %+v", parent)
			}
			guard := &inventoryWriteGuard{Store: f.service.store}
			f.service.store, f.service.inventory.Store = guard, guard
			assertInventoryTestLinks(t, f.service, snapshot, identity.ID(), children)
			assertInventoryTestCounts(t, f.service, snapshot)
			if guard.writes != 0 {
				t.Fatalf("inventory GET repaired missing state with %d writes", guard.writes)
			}
			if variant == "missing_child" {
				child := inventoryTestImage(t, f.service, snapshot, inventory.ID("platform", identity.Authority, identity.Repository, children[0].Digest))
				if child.Category != "unknown" || child.Evidence.ClosureComplete || len(child.Evidence.Reasons) == 0 || child.Platform == nil {
					t.Fatalf("missing child not explicitly unknown: %+v", child)
				}
			}
			newRoot := projectInventoryTestIndex(t, f.service, identity, nil)
			bindInventoryTestDigest(t, f.service, identity, newRoot.Digest)
			refreshTestInventory(t, f.service)
			assertInventoryTestLinks(t, f.service, snapshot, identity.ID(), children)
			if previous := inventoryTestImage(t, f.service, snapshot, identity.ID()); previous.ResolvedDigest != root.Digest {
				t.Fatal("next snapshot changed the previous binding")
			}
		})
	}
}

func projectInventoryTestIndex(t *testing.T, s *Service, identity inventory.Identity, children []artifact.Descriptor) inventory.Content {
	t.Helper()
	if children == nil {
		children = []artifact.Descriptor{}
	}
	body, err := json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	if err != nil {
		t.Fatal(err)
	}
	content, err := s.inventoryContent(context.Background(), identity, artifact.CalculateSHA256(body), ociIndexMediaType, body)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func bindInventoryTestDigest(t *testing.T, s *Service, identity inventory.Identity, digest string) {
	t.Helper()
	if _, err := s.inventoryBinding(context.Background(), identity, digest, "upstream", 0, time.Now().UTC(), true); err != nil {
		t.Fatal(err)
	}
}

func assertInventoryTestLinks(t *testing.T, s *Service, snapshot inventory.Snapshot, id string, children []artifact.Descriptor) {
	t.Helper()
	query := inventory.Query{Kind: "platforms", ImageID: id, SnapshotID: snapshot.SnapshotID, Limit: 1, Filters: url.Values{}}
	seen := map[string]bool{}
	for n := 0; ; n++ {
		page, err := s.inventory.Images(context.Background(), query, s.cursorKey, time.Minute)
		if err != nil || n > len(children) {
			t.Fatalf("platform pagination: %+v %v", page, err)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate platform member")
			}
			seen[item.ID] = true
			inventoryTestImage(t, s, snapshot, item.ID)
		}
		query.Token = page.NextToken
		if query.Token == "" {
			break
		}
	}
	if len(seen) != len(children) {
		t.Fatalf("platform members=%d want=%d", len(seen), len(children))
	}
	for _, path := range []string{
		"/api/v1/images/" + id + "/platforms?snapshotID=" + snapshot.SnapshotID,
		"/api/v1/images?snapshotID=" + snapshot.SnapshotID + "&platform=linux/amd64",
	} {
		w := httptest.NewRecorder()
		s.serveInventory(w, httptest.NewRequest(http.MethodGet, path, nil), context.Background())
		var page inventory.ImagePage
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) == 0 {
			t.Fatalf("GET %s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

func assertInventoryTestCounts(t *testing.T, s *Service, snapshot inventory.Snapshot) {
	t.Helper()
	page, err := s.inventory.Images(context.Background(), inventory.Query{Kind: "images", Scope: "platform", SnapshotID: snapshot.SnapshotID, Limit: 100, Filters: url.Values{}}, s.cursorKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seen, counts := map[string]bool{}, map[string]int64{}
	for _, item := range page.Items {
		if seen[item.ID] {
			t.Fatal("duplicate in platform inventory")
		}
		seen[item.ID] = true
		counts["platform:"+item.Category]++
	}
	if int64(len(seen)) != snapshot.UniqueManifests {
		t.Fatalf("unique manifests=%d want=%d", snapshot.UniqueManifests, len(seen))
	}
	for category, count := range counts {
		if snapshot.Counts[category] != count {
			t.Fatalf("category %s count=%d want=%d", category, snapshot.Counts[category], count)
		}
	}
}
