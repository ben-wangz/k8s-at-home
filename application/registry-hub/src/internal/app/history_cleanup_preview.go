package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

const historyCleanupPreviewPrefix = diagnostics.Prefix + "cleanup-previews/"

func historyCleanupPreviewKey(id string) string { return historyCleanupPreviewPrefix + id + ".json" }

func historyCleanupPrefixes(category string) []string {
	switch category {
	case "diagnostic_events":
		return []string{diagnostics.Prefix + "events/", diagnostics.BatchPrefix}
	case "terminal_work_history":
		return []string{terminalWorkHistoryPrefix, terminalWorkHistoryOrderPrefix, workqueue.Prefix + "receipts/"}
	case "terminal_transfer_history":
		return []string{terminalTransferHistoryPrefix, terminalTransferHistoryOrderPrefix}
	case "storage_snapshot_history":
		return []string{storageInventoryPrefix + "snapshots/"}
	case "full_work_receipts":
		return []string{workqueue.Prefix + "receipts/"}
	case "diagnostic_collection_receipts":
		return []string{diagnostics.Prefix + "collected/"}
	case "transfer_observations":
		return []string{diagnostics.Prefix + "transfers/", diagnostics.Prefix + "transfer-sessions/", diagnostics.Prefix + "upload-receipts/", diagnostics.Prefix + "transfer-progress/"}
	default:
		return nil
	}
}

func (s *Service) createHistoryCleanupPreview(ctx context.Context, principal string, request historyCleanupPreviewRequest) (historyCleanupPreview, error) {
	categories, err := normalizeHistoryCleanupCategories(request.Categories)
	if err != nil || request.Cutoff.IsZero() || request.Cutoff.After(time.Now().UTC()) {
		return historyCleanupPreview{}, storage.ErrCorruptObject
	}
	if err := s.pruneHistoryCleanupPreviews(ctx); err != nil {
		return historyCleanupPreview{}, err
	}
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupPreviewPrefix, "", historyCleanupMaxPreviews+1)
	if err != nil {
		return historyCleanupPreview{}, err
	}
	if len(entries) >= historyCleanupMaxPreviews {
		return historyCleanupPreview{}, errHistoryCleanupCapacity
	}
	cutoff := request.Cutoff.UTC()
	preview := historyCleanupPreview{
		SchemaVersion: 1, ID: diagnostics.ID("history-cleanup-preview", principal, requestID()), Principal: principal,
		Categories: categories, Cutoff: cutoff, PolicyVersion: historyCleanupPolicyVersion,
		Counts: map[string]historyCleanupCounts{}, Complete: true,
		Coverage:  "non-atomic metadata preview; execution rechecks eligibility and source versions",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(s.diagnosticsConfig().HistoryCleanupPreviewTTL),
	}
	if slicesContains(categories, "diagnostic_events") {
		preview.EventBefore = cutoff.Format("20060102")
	}
	remaining := historyCleanupMaxScanned
	for _, category := range categories {
		counts := historyCleanupCounts{SkipReasons: map[string]int64{}, Coverage: historyCleanupCoverage(category)}
		for _, prefix := range historyCleanupPrefixes(category) {
			token := ""
			for {
				limit := min(500, remaining)
				if limit == 0 {
					preview.Complete = false
					break
				}
				page, next, err := storage.Scan(ctx, s.store, prefix, token, limit)
				if err != nil {
					return historyCleanupPreview{}, err
				}
				for _, entry := range page {
					candidate, err := s.classifyHistoryCleanupEntry(ctx, category, entry, cutoff)
					if err != nil {
						return historyCleanupPreview{}, err
					}
					counts.Scanned++
					remaining--
					if candidate.Eligible {
						counts.Eligible++
						counts.EligibleBytes += entry.Size
					} else {
						counts.Protected++
						counts.ProtectedBytes += entry.Size
						counts.SkipReasons[candidate.Reason]++
					}
				}
				if next == "" {
					break
				}
				token = next
			}
			if !preview.Complete {
				break
			}
		}
		preview.Counts[category] = counts
		if !preview.Complete {
			break
		}
	}
	if !preview.Complete {
		preview.Coverage = "truncated at the bounded 100000-object preview limit; execution is disabled"
	}
	data, err := json.Marshal(preview)
	if err != nil {
		return historyCleanupPreview{}, err
	}
	_, err = s.store.Put(ctx, historyCleanupPreviewKey(preview.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		return historyCleanupPreview{}, err
	}
	return preview, nil
}

func normalizeHistoryCleanupCategories(categories []string) ([]string, error) {
	if len(categories) == 0 || len(categories) > len(historyCleanupCategories) {
		return nil, storage.ErrCorruptObject
	}
	allowed := make(map[string]bool, len(historyCleanupCategories))
	for _, category := range historyCleanupCategories {
		allowed[category] = true
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(categories))
	for _, category := range categories {
		if !allowed[category] || seen[category] {
			return nil, storage.ErrCorruptObject
		}
		seen[category] = true
		result = append(result, category)
	}
	slices.Sort(result)
	return result, nil
}

func putHistoryCleanupJSON(ctx context.Context, store storage.Store, key string, value any, options storage.PutOptions) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	return err
}

var errHistoryCleanupCapacity = errors.New("history cleanup operation capacity reached")

func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
