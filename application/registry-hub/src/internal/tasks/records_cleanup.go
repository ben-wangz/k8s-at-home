package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (r *Records) DeleteTaskGraph(ctx context.Context, rootTaskID string, limit int) (int, bool, error) {
	if !validID(rootTaskID) {
		return 0, false, errors.New("root task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	prefix := "solidification/graphs/" + rootTaskID + "/"
	entries, _, err := r.store.List(ctx, prefix, "", limit)
	if err != nil {
		return 0, false, err
	}
	removed := 0
	for _, entry := range entries {
		if err := r.store.DeleteIfMatch(ctx, entry.Key, entry.Version); errors.Is(err, storage.ErrNotFound) {
			continue
		} else if err != nil {
			return removed, false, err
		}
		removed++
	}
	remaining, _, err := r.store.List(ctx, prefix, "", 1)
	if err != nil {
		return removed, false, err
	}
	return removed, len(remaining) == 0, nil
}

func (r *Records) DeleteBlobDemandsForRoot(ctx context.Context, rootTaskID string, limit int) (int, bool, error) {
	if !validID(rootTaskID) {
		return 0, false, errors.New("root task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	prefix := rootBlobDemandPrefix(rootTaskID)
	entries, _, err := r.store.List(ctx, prefix, "", limit)
	if err != nil {
		return 0, false, err
	}
	removed := 0
	for _, entry := range entries {
		object, err := r.store.Get(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return removed, false, err
		}
		var link BlobDemand
		if err := decodeRecord(object, 1<<20, &link); err != nil {
			return removed, false, err
		}
		if link.SchemaVersion != solidificationRecordSchema || link.RootTaskID != rootTaskID || !validID(link.IntentID) || !validID(link.DemandID) {
			return removed, false, errors.New("root blob demand index is invalid")
		}
		forwardKey := blobDemandKey(link.IntentID, link.DemandID)
		forward, version, err := r.readBlobDemand(ctx, forwardKey)
		if err == nil && forward.RootTaskID == rootTaskID {
			if err := r.store.DeleteIfMatch(ctx, forwardKey, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return removed, false, err
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
		if err := r.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
		removed++
	}
	remaining, _, err := r.store.List(ctx, prefix, "", 1)
	if err != nil {
		return removed, false, err
	}
	return removed, len(remaining) == 0, nil
}

func (r *Records) DeleteBlobIntentHistory(ctx context.Context, id string, limit int) (int, bool, error) {
	intent, err := r.GetBlobIntent(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if blobIntentActive(intent.State) || intent.ProtectionSessionID != "" {
		return 0, false, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	prefix := "solidification/blobs/owners/" + id + "/"
	entries, _, err := r.store.List(ctx, prefix, "", limit)
	if err != nil {
		return 0, false, err
	}
	removed := 0
	for _, entry := range entries {
		link, _, err := r.readBlobDemand(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return removed, false, err
		}
		if link.RootTaskID != "" {
			rootIndex := rootBlobDemandIndexKey(link.RootTaskID, link.IntentID, link.DemandID)
			if indexEntry, err := r.store.Head(ctx, rootIndex); err == nil {
				if err := r.store.DeleteIfMatch(ctx, rootIndex, indexEntry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return removed, false, err
				}
			} else if !errors.Is(err, storage.ErrNotFound) {
				return removed, false, err
			}
		}
		if err := r.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
		removed++
	}
	remaining, _, err := r.store.List(ctx, prefix, "", 1)
	if err != nil {
		return removed, false, err
	}
	if len(remaining) > 0 {
		return removed, false, nil
	}
	identityHash, err := recordHash(intent.Identity)
	if err != nil {
		return removed, false, err
	}
	indexKey := "solidification/blobs/index/" + identityHash + ".json"
	index, indexEntry, err := readBlobIntentIndex(ctx, r.store, indexKey)
	if err == nil && index.IntentID == id {
		if err := r.store.DeleteIfMatch(ctx, indexKey, indexEntry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return removed, false, err
	}
	if err := r.store.DeleteIfMatch(ctx, blobIntentKey(id), intent.version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return removed, false, err
	}
	return removed + 1, true, nil
}

func rootBlobDemandPrefix(rootTaskID string) string {
	return "solidification/blobs/root-owners/" + rootTaskID + "/"
}

func rootBlobDemandIndexKey(rootTaskID, intentID, demandID string) string {
	hash, _ := recordHash(struct {
		IntentID string
		DemandID string
	}{intentID, demandID})
	return rootBlobDemandPrefix(rootTaskID) + hash + ".json"
}
