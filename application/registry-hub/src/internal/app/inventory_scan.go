package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type inventoryScan struct {
	Phase             int                       `json:"phase"`
	Token             string                    `json:"token"`
	Complete          bool                      `json:"complete"`
	LastCompleteAt    time.Time                 `json:"lastCompleteAt"`
	Reasons           []string                  `json:"reasons"`
	CatalogRepository string                    `json:"catalogRepository"`
	CatalogLast       string                    `json:"catalogLast"`
	CatalogTags       []string                  `json:"catalogTags,omitempty"`
	CatalogNext       string                    `json:"catalogNext,omitempty"`
	CatalogDone       bool                      `json:"catalogDone,omitempty"`
	Repositories      []string                  `json:"repositories"`
	Page              *inventory.Cursor         `json:"page,omitempty"`
	Root              *inventoryGraphProjection `json:"root,omitempty"`
	Links             []inventory.Content       `json:"links,omitempty"`
	LinkOffset        int                       `json:"linkOffset,omitempty"`
}

func (s *Service) scanInventoryPage(ctx context.Context, state *inventoryScan) error {
	if len(state.Links) > 0 {
		return s.advanceInventoryLinks(ctx, state)
	}
	if state.Phase == 0 && state.Token == "" {
		state.Complete = false
		state.Reasons = appendReason(state.Reasons, "untagged_history_not_enumerable")
	}
	switch state.Phase {
	case 0:
		return s.scanInventoryTasks(ctx, state)
	case 1:
		return s.scanInventoryEntry(ctx, state, "cache/objects/", func(ctx context.Context, key string) error {
			if !strings.Contains(key, "/representations/") || !strings.HasSuffix(key, ".json") {
				return nil
			}
			err := s.scanInventoryCacheEntry(ctx, key)
			if errors.Is(err, storage.ErrCorruptObject) {
				state.Reasons = appendReason(state.Reasons, "cache_metadata_invalid")
				return nil
			}
			return err
		})
	case 2:
		return s.scanInventoryCatalog(ctx, state)
	case 3:
		return s.scanInventoryObservations(ctx, state)
	case 4:
		return s.scanInventoryEntry(ctx, state, inventory.Prefix+"contents/", func(ctx context.Context, key string) error {
			content, _, err := inventory.Read[inventory.Content](ctx, s.store, key)
			if err != nil {
				return err
			}
			if !content.ClosureKnown {
				return s.inventoryDiscoverLocalContent(ctx, content)
			}
			return nil
		})
	default:
		state.Complete = true
		state.LastCompleteAt = time.Now().UTC()
		state.Phase, state.Token, state.Page = 0, "", nil
	}
	return nil
}

func (s *Service) scanInventoryCacheEntry(ctx context.Context, key string) error {
	metadata, _, err := inventory.Read[struct {
		SchemaVersion int       `json:"schemaVersion"`
		Authority     string    `json:"authority"`
		Repository    string    `json:"repository"`
		Digest        string    `json:"digest"`
		MediaType     string    `json:"mediaType"`
		Size          int64     `json:"size"`
		VerifiedAt    time.Time `json:"verifiedAt"`
	}](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if metadata.Authority == "" || metadata.Repository == "" || artifactDigest(metadata.Digest) != nil || metadata.MediaType == "" {
		return storage.ErrCorruptObject
	}
	inspection, err := s.cache.Inspect(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaType)
	if err != nil {
		return err
	}
	identity := inventoryIdentity(metadata.Authority, metadata.Repository, "")
	object := inventory.Object{ID: identity.ContentID(metadata.Digest), Identity: identity, ObjectRef: inventory.ObjectRef{Role: "blob", Digest: metadata.Digest, MediaType: metadata.MediaType, Size: inspection.Size}, CacheState: "none", DataVersion: inspection.DataVersion, CleanupPending: inspection.State == "cleanup_pending", CheckedAt: time.Now().UTC(), Reasons: []string{}, TaskIDs: []string{}}
	if inventoryManifestType(metadata.MediaType) {
		object.Role = "manifest"
	}
	if inspection.Present {
		object.CacheState = "complete"
	}
	if inspection.Unknown {
		object.CacheState = "unknown"
	}
	if inspection.Reason != "" {
		object.Reasons = append(object.Reasons, inspection.Reason)
	}
	if err := inventory.Update(ctx, s.store, inventory.Prefix+"cache/"+object.ID+".json", func(value *inventory.Object) error { *value = object; return nil }); err != nil {
		return err
	}
	if !inspection.Present && !inspection.Unknown || !inventoryManifestType(metadata.MediaType) {
		return nil
	}
	representation := artifact.CachedRepresentation{Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest, MediaType: metadata.MediaType, Size: metadata.Size}
	data, err := s.cache.ReadManifestForInventory(ctx, representation, s.cfg.Limits.ManifestBytes)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	content, err := s.inventoryContent(ctx, identity, metadata.Digest, metadata.MediaType, data)
	if err != nil {
		return err
	}
	knownNew := inspection.InventoryTracked
	if err := s.inventory.Register(ctx, inventory.LifecycleVersion(inventory.ID("platform", identity.Authority, identity.Repository, content.Digest), "", content.Generation), knownNew); err != nil {
		return err
	}
	_, err = s.inventoryBinding(ctx, inventoryIdentity(identity.Authority, identity.Repository, metadata.Digest), metadata.Digest, "cache", content.Generation, metadata.VerifiedAt, knownNew)
	return err
}

func inventoryManifestType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.index.v1+json" || mediaType == "application/vnd.oci.image.manifest.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

func appendReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
