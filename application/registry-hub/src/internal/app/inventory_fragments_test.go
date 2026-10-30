package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
)

func inventoryFragmentFixture(t *testing.T) (*stagedAcceptanceFixture, inventoryFragment) {
	t.Helper()
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	manifest, _, _ := publishAnchoredTestPlatform(f, "fragment")
	refreshTestInventory(t, f.service)
	id := inventory.ID("platform", "ghcr.io", "team/image", manifest.Digest)
	fragment, _, err := inventory.Read[inventoryFragment](context.Background(), f.service.store, inventory.Prefix+"fragments/image/"+id+".json")
	if err != nil || fragment.Dependencies.Invalid || len(fragment.Dependencies.Objects) == 0 {
		t.Fatalf("completed image has no reusable fragment: invalid=%v dependencies=%d error=%v", fragment.Dependencies.Invalid, len(fragment.Dependencies.Objects), err)
	}
	return f, fragment
}

func resumeInventoryFragment(t *testing.T, s *Service, fragment inventoryFragment) *inventoryImageProgress {
	t.Helper()
	ctx := context.Background()
	content, err := s.inventory.Content(ctx, fragment.Image.Identity, fragment.Image.ResolvedDigest)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.newInventoryImageProgress(ctx, fragment.Image, content)
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; p.Reuse != nil && step < 10000; step++ {
		if err := s.advanceInventoryFragment(ctx, p); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		p = &inventoryImageProgress{}
		if err := json.Unmarshal(data, p); err != nil {
			t.Fatal(err)
		}
	}
	if p.Reuse != nil {
		t.Fatal("fragment validation did not converge")
	}
	return p
}

func TestInventoryFragmentReplaysValidationWithoutRebuildingStableImage(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	p := resumeInventoryFragment(t, f.service, fragment)
	if p.Phase != 6 || p.Image.ID != fragment.Image.ID || !p.Image.Evidence.CheckedAt.Equal(fragment.Image.Evidence.CheckedAt) {
		for key, expected := range fragment.Dependencies.Objects {
			actual, err := f.service.inventoryDependencyVersion(context.Background(), key, expected)
			if errors.Is(err, storage.ErrNotFound) {
				actual = "missing"
			}
			if actual != expected {
				t.Logf("changed dependency %s: expected=%s actual=%s error=%v", key, expected, actual, err)
			}
		}
		for _, listing := range fragment.Dependencies.Listings {
			entries, next, err := f.service.inventoryDependencyListing(context.Background(), listing)
			if listingFingerprint(entries, next) != listing.Fingerprint {
				t.Logf("changed listing %s: entries=%+v error=%v", listing.Prefix, entries, err)
			}
		}
		t.Fatalf("unchanged image was rebuilt or its observation time advanced: phase=%d image=%+v", p.Phase, p.Image)
	}
}

func TestInventoryFragmentRejectsChangedSourceVersions(t *testing.T) {
	for _, prefix := range []string{"cache/objects/", "tasks/state/", inventory.Prefix + "proofs/", inventory.Prefix + "contents/"} {
		t.Run(prefix, func(t *testing.T) {
			f, fragment := inventoryFragmentFixture(t)
			key := ""
			for candidate, version := range fragment.Dependencies.Objects {
				if strings.HasPrefix(candidate, prefix) && version != "missing" && !strings.HasPrefix(version, "fingerprint:") {
					key = candidate
					break
				}
			}
			if key == "" {
				t.Fatalf("fragment omitted %s dependencies", prefix)
			}
			if err := inventory.Update(context.Background(), f.service.store, key, func(value *map[string]any) error {
				(*value)["sourceVersionTest"] = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if p := resumeInventoryFragment(t, f.service, fragment); p.Phase != 0 {
				t.Fatalf("changed source reused frozen state: phase=%d key=%s", p.Phase, key)
			}
		})
	}
}

func TestInventoryFragmentRejectsUntrackedPayloadCorruptionAndEpochChange(t *testing.T) {
	for _, epoch := range []bool{false, true} {
		t.Run(map[bool]string{false: "payload", true: "gc_epoch"}[epoch], func(t *testing.T) {
			f, fragment := inventoryFragmentFixture(t)
			ctx := context.Background()
			if epoch {
				if err := inventory.Update(ctx, f.service.store, "system/gc/state.json", func(value *gateRecord) error { value.Epoch++; return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				key := ""
				for candidate, version := range fragment.Dependencies.Objects {
					if strings.HasPrefix(version, "fingerprint:") {
						key = candidate
						break
					}
				}
				if key == "" {
					t.Fatal("fragment omitted payload fingerprints")
				}
				// Bypass notifications to model corruption outside the application.
				if err := f.service.store.(*observedStore).Store.Delete(ctx, key); err != nil {
					t.Fatal(err)
				}
			}
			if p := resumeInventoryFragment(t, f.service, fragment); p.Phase != 0 {
				t.Fatalf("changed physical evidence reused: phase=%d", p.Phase)
			}
		})
	}
}

func TestInventoryFragmentRejectsNewWorkAndBindingIdentity(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	key := inventory.Prefix + "content-work/" + fragment.Image.ContentID + "/new.json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryWork) error { value.Work.ID = "new"; return nil }); err != nil {
		t.Fatal(err)
	}
	if p := resumeInventoryFragment(t, f.service, fragment); p.Phase != 0 {
		t.Fatal("new work inserted into a previously captured list was ignored")
	}
	image := fragment.Image
	image.Scope, image.ID, image.Reference = "reference", inventory.ID("another-binding"), "another-tag"
	image.ResolvedDigest = "sha256:" + strings.Repeat("1", 64)
	p := inventoryImageProgress{Image: image, Epoch: fragment.Epoch}
	if err := f.service.loadInventoryFragment(ctx, &p); err != nil || p.Reuse != nil {
		t.Fatalf("new binding adopted another content identity: reuse=%+v error=%v", p.Reuse, err)
	}
}

func TestInventoryContentFragmentPreservesNewReferenceIdentity(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	fragment.Image.Identity.Reference = "another-tag"
	fragment.Image.ID = fragment.Image.Identity.ID()
	fragment.Image.Scope = "reference"
	fragment.Image.BindingRevision = "another-binding-revision"
	p := resumeInventoryFragment(t, f.service, fragment)
	if p.Phase != 2 || p.Image.ID != fragment.Image.ID || p.Image.Reference != "another-tag" || p.Image.BindingRevision != fragment.Image.BindingRevision {
		t.Fatalf("content reuse changed the new binding identity: %+v", p.Image)
	}
	for _, object := range p.Objects {
		if object.Identity != p.Image.Identity {
			t.Fatalf("shared content retained another image's object identity: %+v", object.Identity)
		}
	}
}
