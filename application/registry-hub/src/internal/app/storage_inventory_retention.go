package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

const storageRetentionPrefix = storageInventoryPrefix + "retention/"

type storageRetention struct {
	SnapshotID      string    `json:"snapshotID"`
	ManifestVersion string    `json:"manifestVersion"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Token           string    `json:"token"`
	ChildrenDone    bool      `json:"childrenDone"`
}

func (s *Service) storageCollectionActive(ctx context.Context, id string) (bool, error) {
	checkpoint, _, err := inventory.Read[storageCollection](ctx, s.store, storageInventoryPrefix+"checkpoint.json")
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return checkpoint.Snapshot.SnapshotID == id, err
}

func (s *Service) cleanupStorageObservation(ctx context.Context, entry storage.Entry) error {
	parts := strings.Split(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	if len(parts) < 2 || !inventory.ValidID(parts[0]) {
		return nil
	}
	id := parts[0]
	if active, err := s.storageCollectionActive(ctx, id); err != nil || active {
		return err
	}
	_, _, err := inventory.Read[storageRetention](ctx, s.store, storageRetentionPrefix+id+".json")
	if err == nil {
		return s.advanceStorageRetention(ctx, id)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	prefix := storageInventoryPrefix + "snapshots/" + id + "/"
	snapshot, version, err := inventory.Read[storageSnapshot](ctx, s.store, prefix+"manifest.json")
	if errors.Is(err, storage.ErrNotFound) {
		if entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < 24*time.Hour {
			return nil
		}
		return s.deleteStorageRetentionEntry(ctx, entry)
	}
	if err != nil {
		return err
	}
	if snapshot.SnapshotID != id || snapshot.ExpiresAt.IsZero() {
		return storage.ErrCorruptObject
	}
	if snapshot.ExpiresAt.After(time.Now()) {
		// Published reports only query the final usage run; intermediate runs are disposable.
		if snapshot.UsageVersion > 0 && strings.HasPrefix(entry.Key, prefix+"usage/") && !strings.HasPrefix(entry.Key, usageRunPrefix(id, snapshot.UsageRun)) {
			return s.deleteStorageRetentionEntry(ctx, entry)
		}
		return nil
	}
	plan := storageRetention{SnapshotID: id, ManifestVersion: version, ExpiresAt: snapshot.ExpiresAt}
	if err := inventory.Update(ctx, s.store, storageRetentionPrefix+id+".json", func(value *storageRetention) error {
		if value.SnapshotID == "" {
			*value = plan
		}
		return nil
	}); err != nil {
		return err
	}
	return s.advanceStorageRetention(ctx, id)
}

func (s *Service) advanceStorageRetention(ctx context.Context, id string) error {
	_, err := s.advanceStorageRetentionBefore(ctx, id, time.Now().UTC())
	return err
}

func (s *Service) advanceStorageRetentionBefore(ctx context.Context, id string, cutoff time.Time) (historyCleanupEntryProgress, error) {
	progress := historyCleanupEntryProgress{}
	if active, err := s.storageCollectionActive(ctx, id); err != nil || active {
		return progress, err
	}
	key := storageRetentionPrefix + id + ".json"
	plan, version, err := inventory.Read[storageRetention](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		snapshot, manifestVersion, readErr := inventory.Read[storageSnapshot](ctx, s.store, storageInventoryPrefix+"snapshots/"+id+"/manifest.json")
		if errors.Is(readErr, storage.ErrNotFound) {
			return progress, nil
		}
		if readErr != nil {
			return progress, readErr
		}
		if snapshot.SnapshotID != id || snapshot.ExpiresAt.IsZero() {
			return progress, storage.ErrCorruptObject
		}
		if snapshot.ExpiresAt.After(cutoff) {
			progress.Skipped++
			return progress, nil
		}
		candidate := storageRetention{SnapshotID: id, ManifestVersion: manifestVersion, ExpiresAt: snapshot.ExpiresAt}
		if err := inventory.Update(ctx, s.store, key, func(current *storageRetention) error {
			if current.SnapshotID == "" {
				*current = candidate
			}
			return nil
		}); err != nil {
			return progress, err
		}
		plan, version, err = inventory.Read[storageRetention](ctx, s.store, key)
		if err != nil {
			return progress, err
		}
	}
	if err != nil {
		return progress, err
	}
	if plan.SnapshotID != id || plan.ExpiresAt.IsZero() || plan.ExpiresAt.After(cutoff) || plan.ManifestVersion == "" {
		return progress, storage.ErrCorruptObject
	}
	prefix := storageInventoryPrefix + "snapshots/" + id + "/"
	manifest := prefix + "manifest.json"
	current, err := s.store.Head(ctx, manifest)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return progress, err
	}
	if err == nil && current.Version != plan.ManifestVersion {
		return progress, storage.ErrConditionFailed
	}
	if !plan.ChildrenDone {
		previous := plan
		entries, next, err := storage.Scan(ctx, s.store, prefix, plan.Token, 32)
		if err != nil {
			return progress, err
		}
		for _, child := range entries {
			if child.Key == manifest {
				continue
			}
			deleted, missing, err := s.deleteHistoryEntryConditional(ctx, child)
			if err != nil {
				return progress, err
			}
			if deleted {
				progress.Deleted++
				progress.DeletedBytes += child.Size
			} else if missing {
				progress.Missing++
			}
		}
		plan.Token, plan.ChildrenDone = next, next == ""
		err = inventory.Update(ctx, s.store, key, func(value *storageRetention) error {
			if *value != previous && *value != plan {
				return storage.ErrConditionFailed
			}
			*value = plan
			return nil
		})
		return progress, err
	}
	if err == nil {
		deleted, missing, err := s.deleteHistoryEntryConditional(ctx, current)
		if err != nil {
			return progress, err
		}
		if deleted {
			progress.Deleted++
			progress.DeletedBytes += current.Size
		} else if missing {
			progress.Missing++
		}
	} else {
		progress.Missing++
	}
	err = s.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) {
		err = nil
	}
	return progress, err
}

func (s *Service) deleteHistoryEntryConditional(ctx context.Context, entry storage.Entry) (bool, bool, error) {
	if entry.Version == "" {
		current, err := s.store.Head(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		if entry.ModTime != current.ModTime || entry.Size != current.Size {
			return false, false, storage.ErrConditionFailed
		}
		entry = current
	}
	err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return false, true, nil
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, headErr := s.store.Head(ctx, entry.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return true, false, nil
		}
		if headErr != nil {
			return false, false, errors.Join(err, headErr)
		}
		if sourceEntryVersion(current) != sourceEntryVersion(entry) {
			return false, false, storage.ErrConditionFailed
		}
	}
	return err == nil, false, err
}

func (s *Service) deleteStorageRetentionEntry(ctx context.Context, entry storage.Entry) error {
	err := s.deleteDiagnosticObservation(ctx, entry)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
