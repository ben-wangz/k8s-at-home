package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) recheckHistoryCleanupCandidate(ctx context.Context, preview historyCleanupRangePreview, entry storage.Entry, ref historyCleanupCandidateRef) (historyCleanupRangeCandidate, bool, error) {
	if preview.Category != "storage_snapshot_history" {
		if age := s.historyCleanupMinimumAge(preview.Category); age > 0 {
			preview.EffectiveRange = s.effectiveHistoryCleanupRange(preview.Category, preview.EffectiveRange, time.Now().UTC())
		}
		candidate, inRange, err := s.classifyHistoryCleanupRangeEntry(ctx, preview, entry, &historyCleanupRangeProgress{})
		if err == nil && candidate.Eligible && (candidate.Reference.Key != ref.Key ||
			historyCleanupReferenceChanged(ref, historyCleanupCandidateEntry(candidate.Reference))) {
			return historyCleanupRangeCandidate{Reason: "version_changed"}, true, nil
		}
		return candidate, inRange, err
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	snapshot, err := s.readHistoryCleanupSnapshot(ctx, id, preview.EffectiveRange)
	if err != nil || snapshot.Manifest == nil {
		return historyCleanupRangeCandidate{Reason: snapshot.Reason}, !snapshot.Outside, err
	}
	expected := ref.Manifest
	if expected == nil {
		expected = &ref
	}
	if expected.Key != snapshot.Manifest.Key || expected.Version != snapshot.Manifest.Version {
		return historyCleanupRangeCandidate{Reason: "version_changed"}, true, nil
	}
	return historyCleanupRangeCandidate{Eligible: true}, true, nil
}

func (s *Service) deleteHistoryCleanupSnapshotCandidate(ctx context.Context, current storage.Entry, ref historyCleanupCandidateRef) (historyCleanupEntryProgress, error) {
	manifestRef := &ref
	if ref.Manifest != nil {
		manifestRef = ref.Manifest
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(manifestRef.Key, storageInventoryPrefix+"snapshots/"), "/")
	retentionKey := storageRetentionPrefix + id + ".json"
	if err := inventory.Update(ctx, s.store, retentionKey, func(plan *storageRetention) error {
		if plan.SnapshotID == "" {
			*plan = storageRetention{SnapshotID: id, ManifestVersion: manifestRef.Version, ExpiresAt: manifestRef.At}
		}
		if plan.SnapshotID != id || plan.ManifestVersion != manifestRef.Version {
			return storage.ErrConditionFailed
		}
		return nil
	}); err != nil {
		return historyCleanupEntryProgress{}, err
	}
	if ref.Manifest != nil {
		manifest, err := s.store.Head(ctx, ref.Manifest.Key)
		if errors.Is(err, storage.ErrNotFound) || err == nil && historyCleanupReferenceChanged(*ref.Manifest, manifest) {
			return historyCleanupEntryProgress{}, storage.ErrConditionFailed
		}
		if err != nil {
			return historyCleanupEntryProgress{}, err
		}
		return s.deleteHistoryCleanupObject(ctx, current)
	}
	entries, next, err := storage.Scan(ctx, s.store, strings.TrimSuffix(current.Key, "manifest.json"), "", 2)
	if err != nil {
		return historyCleanupEntryProgress{}, err
	}
	for _, entry := range entries {
		if entry.Key != current.Key {
			return historyCleanupEntryProgress{Skipped: 1}, nil
		}
	}
	if next != "" {
		return historyCleanupEntryProgress{Skipped: 1}, nil
	}
	return s.deleteHistoryCleanupObject(ctx, current)
}

func (s *Service) deleteHistoryCleanupSnapshotPlan(ctx context.Context, manifestRef historyCleanupCandidateRef) error {
	id, _, _ := strings.Cut(strings.TrimPrefix(manifestRef.Key, storageInventoryPrefix+"snapshots/"), "/")
	retentionKey := storageRetentionPrefix + id + ".json"
	plan, version, err := inventory.Read[storageRetention](ctx, s.store, retentionKey)
	if err == nil && plan.ManifestVersion == manifestRef.Version {
		err = s.store.DeleteIfMatch(ctx, retentionKey, version)
	}
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}
