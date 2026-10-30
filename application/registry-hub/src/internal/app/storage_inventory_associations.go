package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type storageAssociation struct {
	ImageID          string              `json:"imageID"`
	CanonicalImageID string              `json:"canonicalImageID"`
	Authority        string              `json:"authority"`
	Repository       string              `json:"repository"`
	Platform         *inventory.Platform `json:"platform"`
}

type storageAssociationSet struct {
	Items         []storageAssociation `json:"items"`
	Truncated     bool                 `json:"truncated"`
	DeclaredBytes int64                `json:"declaredBytes"`
}

func storageAssociationKey(snapshot, domain, identity string) string {
	return storageInventoryPrefix + "snapshots/" + snapshot + "/associations/" + domain + "/" + inventory.ID("physical-association", identity) + ".json"
}

func cacheStorageIdentity(authority, repository, digest string) string {
	hash := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + digest))
	return hex.EncodeToString(hash[:])
}

func (s *Service) advanceStorageAttribution(ctx context.Context, state *storageCollection) error {
	if state.AttributionPhase == 0 {
		snapshot, err := s.inventory.Snapshot(ctx, "", s.inventoryConfig().StaleAfter)
		if err != nil {
			state.Snapshot.AttributionCoverage = "unknown"
			state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "inventory_attribution_"+diagnosticReason(err))
			state.AttributionPhase = 3
			return nil
		}
		state.Snapshot.InventorySnapshotID = snapshot.SnapshotID
		state.Snapshot.InventoryObservedAt = snapshot.ObservedAt
		state.Snapshot.AttributionCoverage = "captured_inventory; external or unindexed references remain unknown"
		state.AttributionPhase = 1
		return nil
	}
	if state.AttributionPhase == 2 {
		return s.advanceStorageLogicalUsage(ctx, state)
	}
	if _, err := s.inventory.Snapshot(ctx, state.Snapshot.InventorySnapshotID, s.inventoryConfig().StaleAfter); err != nil {
		return storageAttributionFailure(state, err)
	}
	if state.AttributionImage == nil {
		entry, ok, err := state.AttributionCursor.Peek(ctx, s.store, inventory.Prefix+"snapshots/"+state.Snapshot.InventorySnapshotID+"/members/", 32)
		if err != nil {
			return err
		}
		if !ok {
			if state.AttributionCursor.Done {
				state.AttributionPhase = 2
			}
			return nil
		}
		member, _, err := inventory.Read[struct {
			ID string `json:"id"`
		}](ctx, s.store, entry.Key)
		if err != nil {
			return storageAttributionFailure(state, err)
		}
		image, err := s.inventory.Image(ctx, inventory.Snapshot{SnapshotID: state.Snapshot.InventorySnapshotID}, member.ID)
		if err != nil {
			return storageAttributionFailure(state, err)
		}
		pages, _, err := inventory.Read[int](ctx, s.store, inventory.Prefix+"snapshots/"+state.Snapshot.InventorySnapshotID+"/objects/"+image.ID+"/count.json")
		if err != nil {
			return storageAttributionFailure(state, err)
		}
		state.AttributionImage = &image
		state.AttributionPages = pages
		state.AttributionPage = 0
		state.AttributionOffset = 0
		return nil
	}
	image := state.AttributionImage
	if state.AttributionPage >= state.AttributionPages {
		state.AttributionImage = nil
		state.AttributionCursor.Advance()
		return nil
	}
	objects, _, err := inventory.Read[[]inventory.Object](ctx, s.store, fmt.Sprintf("%ssnapshots/%s/objects/%s/%08d.json", inventory.Prefix, state.Snapshot.InventorySnapshotID, image.ID, state.AttributionPage))
	if err != nil {
		return storageAttributionFailure(state, err)
	}
	end := min(state.AttributionOffset+4, len(objects))
	for _, object := range objects[state.AttributionOffset:end] {
		association := storageAssociation{ImageID: image.ID, CanonicalImageID: image.ContentID, Authority: image.Authority, Repository: image.Repository, Platform: image.Platform}
		for domain, identity := range map[string]string{"state": cacheStorageIdentity(object.Authority, object.Repository, object.Digest), "registry": object.Digest} {
			if err := inventory.Update(ctx, s.store, storageAssociationKey(state.Snapshot.SnapshotID, domain, identity), func(set *storageAssociationSet) error {
				set.DeclaredBytes = object.Size
				for _, existing := range set.Items {
					if existing.ImageID == association.ImageID {
						return nil
					}
				}
				if len(set.Items) >= 128 {
					set.Truncated = true
					return nil
				}
				set.Items = append(set.Items, association)
				return nil
			}); err != nil {
				return err
			}
		}
	}
	state.AttributionOffset = end
	if end >= len(objects) {
		state.AttributionPage++
		state.AttributionOffset = 0
	}
	return nil
}

func storageAttributionFailure(state *storageCollection, err error) error {
	if !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, inventory.ErrExpired) {
		return err
	}
	state.AttributionPhase = 3
	state.Snapshot.AttributionCoverage = "partial; retained inventory source expired during collection"
	state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "inventory_attribution_source_expired")
	return nil
}

func (s *Service) attachStorageAssociations(ctx context.Context, snapshot, domain, identity string, object *storageObject) {
	set, _, err := inventory.Read[storageAssociationSet](ctx, s.store, storageAssociationKey(snapshot, domain, identity))
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			object.ReclaimReason = "association_" + diagnosticReason(err)
		}
		return
	}
	object.Associations, object.AssociationsTruncated = set.Items, set.Truncated
	owners := map[string]bool{}
	for _, association := range set.Items {
		owners[association.CanonicalImageID] = true
	}
	if len(owners) > 1 {
		shared := true
		object.Shared = &shared
	} else if len(owners) == 1 && !set.Truncated {
		shared := false
		object.Shared = &shared
	}
	if len(set.Items) > 0 {
		object.AssociationState = "known"
	}
}
