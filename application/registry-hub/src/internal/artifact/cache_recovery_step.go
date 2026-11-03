package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sort"
	"strings"
)

type RepresentationRecovery struct {
	Cursor   inventory.Cursor          `json:"cursor"`
	Identity string                    `json:"identity,omitempty"`
	Index    *cacheRepresentationIndex `json:"index,omitempty"`
	Done     bool                      `json:"done"`
}

// RebuildRepresentationStep requires the caller to pause cache mutations for the generation.
func (c *Cache) RebuildRepresentationStep(ctx context.Context, state *RepresentationRecovery) error {
	entry, ok, err := state.Cursor.Peek(ctx, c.store, "cache/objects/", 16)
	if err != nil {
		return err
	}
	identity := ""
	if ok {
		identity, _, _ = strings.Cut(strings.TrimPrefix(entry.Key, "cache/objects/"), "/")
	}
	if state.Identity != "" && (identity != state.Identity || state.Cursor.Done) {
		if state.Index == nil {
			err = c.deleteRepresentationIndex(ctx, state.Identity)
		} else {
			sort.Strings(state.Index.MediaTypes)
			err = c.replaceRepresentationIndex(ctx, state.Identity, *state.Index)
		}
		if err != nil {
			return err
		}
		state.Identity, state.Index = "", nil
		return nil
	}
	if !ok {
		state.Done = state.Cursor.Done
		return nil
	}
	state.Identity = identity
	if strings.Contains(entry.Key, "/representations/") && strings.HasSuffix(entry.Key, ".json") {
		if err := c.recoverRepresentation(ctx, state, entry.Key); err != nil {
			return err
		}
	}
	state.Cursor.Advance()
	return nil
}

func (c *Cache) recoverRepresentation(ctx context.Context, state *RepresentationRecovery, key string) error {
	metadata, _, err := c.readMetadataEntry(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Authority == "" || metadata.Repository == "" || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 || metadata.DataVersion == "" || cacheIdentity(metadata.Authority, metadata.Repository, metadata.Digest) != state.Identity {
		return storage.ErrCorruptObject
	}
	if _, err := Hasher(metadata.Digest); err != nil {
		return errors.Join(storage.ErrCorruptObject, err)
	}
	if metadata.State != "complete" && metadata.State != "cleanup_pending" {
		return nil
	}
	if err := c.verifyComplete(ctx, state.Identity, metadata); errors.Is(err, storage.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if state.Index == nil {
		state.Index = &cacheRepresentationIndex{SchemaVersion: cacheSchemaCurrent, Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest}
	}
	for _, mediaType := range state.Index.MediaTypes {
		if mediaType == metadata.MediaType {
			return nil
		}
	}
	if len(state.Index.MediaTypes) >= cacheRepresentationIndexMaxMediaTypes {
		return storage.ErrCorruptObject
	}
	state.Index.MediaTypes = append(state.Index.MediaTypes, metadata.MediaType)
	return nil
}
