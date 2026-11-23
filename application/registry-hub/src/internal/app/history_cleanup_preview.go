package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
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

func (s *Service) classifyHistoryCleanupEntry(ctx context.Context, category string, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	switch category {
	case "diagnostic_events":
		date, ok := historyCleanupEventDate(entry.Key)
		if !ok {
			return historyCleanupCandidate{Reason: "unknown_event_partition"}, nil
		}
		return historyCleanupCandidate{Eligible: date < cutoff.Format("20060102"), Reason: "unsealed_partition"}, nil
	case "terminal_work_history":
		return s.classifyTerminalWorkEntry(ctx, entry, cutoff)
	case "terminal_transfer_history":
		return s.classifyTerminalTransferEntry(ctx, entry, cutoff)
	case "storage_snapshot_history":
		return s.classifyStorageSnapshotEntry(ctx, entry, cutoff)
	default:
		return historyCleanupCandidate{Reason: "unsupported_category"}, nil
	}
}

func (s *Service) classifyTerminalWorkEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	switch {
	case strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix):
		record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, entry.Key)
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		if !validTerminalWorkHistory(record, entry.Key) {
			return historyCleanupCandidate{Reason: "invalid_history_record"}, nil
		}
		return historyCleanupCandidate{Eligible: !record.TerminalAt.After(cutoff), Reason: "after_cutoff"}, nil
	case strings.HasPrefix(entry.Key, terminalWorkHistoryOrderPrefix):
		at, ok := historyOrderTime(terminalWorkHistoryOrderPrefix, entry.Key)
		return historyCleanupCandidate{Eligible: ok && !at.After(cutoff), Reason: "after_cutoff"}, nil
	case strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/"):
		return s.classifyWorkReceiptEntry(ctx, entry, cutoff)
	default:
		return historyCleanupCandidate{Reason: "outside_history_scope"}, nil
	}
}

func (s *Service) classifyWorkReceiptEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	_, id, ok := receiptWorkIdentity(entry.Key)
	if !ok {
		return historyCleanupCandidate{Reason: "invalid_receipt_key"}, nil
	}
	receipt, _, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupCandidate{Reason: "receipt_missing"}, nil
	}
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if receipt.ID != id || receipt.State != "done" {
		return historyCleanupCandidate{Reason: "active_receipt"}, nil
	}
	terminalAt := receipt.CompletedAt
	if terminalAt.IsZero() && entry.ModTime != 0 {
		terminalAt = time.Unix(0, entry.ModTime).UTC()
	}
	if terminalAt.IsZero() || terminalAt.After(cutoff) {
		return historyCleanupCandidate{Reason: "after_cutoff"}, nil
	}
	if time.Since(terminalAt) < s.diagnosticsConfig().FullWorkReceiptRetention {
		return historyCleanupCandidate{Reason: "minimum_receipt_retention"}, nil
	}
	var progress sourceSweepCheckpoint
	if err := s.cleanupWorkReceipt(ctx, entry, &progress, true); err != nil {
		return historyCleanupCandidate{}, err
	}
	if progress.Status.Counts["eligible"] > 0 {
		return historyCleanupCandidate{Eligible: true}, nil
	}
	for _, reason := range []string{"protected_active_work", "protected_execution_sources_unverified", "protected_execution_fact_pending", "protected_outbox_pending", "protected_version_changed"} {
		if progress.Status.Counts[reason] > 0 {
			return historyCleanupCandidate{Reason: reason}, nil
		}
	}
	return historyCleanupCandidate{Reason: "receipt_not_eligible"}, nil
}

func (s *Service) classifyTerminalTransferEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	if strings.HasPrefix(entry.Key, terminalTransferHistoryOrderPrefix) {
		at, ok := historyOrderTime(terminalTransferHistoryOrderPrefix, entry.Key)
		return historyCleanupCandidate{Eligible: ok && !at.After(cutoff), Reason: "after_cutoff"}, nil
	}
	record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, entry.Key)
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if !validTerminalTransferHistory(record, entry.Key) {
		return historyCleanupCandidate{Reason: "invalid_history_record"}, nil
	}
	return historyCleanupCandidate{Eligible: !record.TerminalAt.After(cutoff), Reason: "after_cutoff"}, nil
}

func (s *Service) classifyStorageSnapshotEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	parts := strings.Split(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	if len(parts) < 2 || !inventory.ValidID(parts[0]) {
		return historyCleanupCandidate{Reason: "invalid_snapshot_key"}, nil
	}
	if active, err := s.storageCollectionActive(ctx, parts[0]); err != nil || active {
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		return historyCleanupCandidate{Reason: "active_snapshot"}, nil
	}
	snapshot, _, err := inventory.Read[storageSnapshot](ctx, s.store, storageInventoryPrefix+"snapshots/"+parts[0]+"/manifest.json")
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupCandidate{Reason: "snapshot_manifest_missing"}, nil
	}
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if snapshot.SnapshotID != parts[0] || snapshot.ExpiresAt.IsZero() {
		return historyCleanupCandidate{Reason: "invalid_snapshot_manifest"}, nil
	}
	return historyCleanupCandidate{Eligible: !snapshot.ExpiresAt.After(cutoff), Reason: "live_snapshot"}, nil
}

func historyCleanupEventDate(key string) (string, bool) {
	var date string
	switch {
	case strings.HasPrefix(key, diagnostics.Prefix+"events/"):
		parts := strings.Split(strings.TrimPrefix(key, diagnostics.Prefix+"events/"), "/")
		if len(parts) < 2 {
			return "", false
		}
		date = parts[0]
	case strings.HasPrefix(key, diagnostics.BatchPrefix):
		parts := strings.Split(strings.TrimPrefix(key, diagnostics.BatchPrefix), "/")
		if len(parts) < 2 {
			return "", false
		}
		date = parts[0]
	default:
		return "", false
	}
	parsed, err := time.Parse("20060102", date)
	return date, err == nil && parsed.UTC().Format("20060102") == date
}

func historyCleanupCoverage(category string) string {
	switch category {
	case "diagnostic_events":
		return "legacy events and validated batch objects; whole UTC partitions before the cutoff date only"
	case "terminal_work_history":
		return "terminal projections and full receipts passing receipt, reference, outbox, and execution-fact guards"
	case "terminal_transfer_history":
		return "terminal history projections and their order indexes; source transfer state is protected"
	case "storage_snapshot_history":
		return "expired snapshot contents using the manifest, cursor, active-build, and conditional-delete checks"
	default:
		return "unknown"
	}
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
