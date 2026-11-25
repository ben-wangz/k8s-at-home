package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) readHistoryCleanupEligibleVolume(ctx context.Context, view *historyCleanupRetentionView) error {
	latest := map[string]time.Time{}
	record := func(category string, counts historyCleanupCounts, createdAt time.Time) {
		if createdAt.After(latest[category]) {
			view.KnownEligibleVolume[category], latest[category] = counts, createdAt
		}
	}
	now := view.ObservedAt
	if err := s.scanHistoryCleanupRecords(ctx, historyCleanupPreviewPrefix, func(entry storage.Entry) error {
		preview, _, err := inventory.Read[historyCleanupPreview](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if preview.Complete && preview.ExpiresAt.After(now) {
			for category, counts := range preview.Counts {
				record(category, counts, preview.CreatedAt)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return s.scanHistoryCleanupRecords(ctx, historyCleanupRangePrefix, func(entry storage.Entry) error {
		if strings.HasSuffix(entry.Key, ".progress.json") {
			return nil
		}
		preview, _, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if preview.Status == "completed" && preview.ExpiresAt.After(now) {
			record(preview.Category, preview.Counts, preview.CreatedAt)
		}
		return nil
	})
}
