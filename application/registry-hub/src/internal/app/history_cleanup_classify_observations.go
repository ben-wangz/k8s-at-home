package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

func (s *Service) classifyCollectionReceiptEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupCandidate{Reason: "receipt_missing"}, nil
	}
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	at := receipt.UpdatedAt
	if at.IsZero() && entry.ModTime > 0 {
		at = time.Unix(0, entry.ModTime).UTC()
	}
	if at.IsZero() || at.After(cutoff) || time.Since(at) < s.diagnosticsConfig().CollectedReceiptRetention {
		return historyCleanupCandidate{Reason: "minimum_collection_retention"}, nil
	}
	if safe, err := s.diagnosticCollectionReceiptRemovable(ctx, entry.Key, receipt); !safe || err != nil {
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		return historyCleanupCandidate{Reason: "handoff_pending"}, nil
	}
	return historyCleanupCandidate{Eligible: true}, nil
}

func (s *Service) diagnosticCollectionReceiptRemovable(ctx context.Context, key string, receipt eventCollectionReceipt) (bool, error) {
	if !diagnosticCollectionReceiptSettled(receipt) {
		return false, nil
	}
	if len(receipt.Dispositions) == 0 {
		return true, nil
	}
	source := receipt.Dispositions[0].SourceKey
	if key != diagnostics.Prefix+"collected/"+diagnostics.ID(source)+".json" {
		return false, nil
	}
	_, pending, err := s.pendingDiagnosticSource(ctx, source)
	if pending || err != nil {
		return false, err
	}
	if strings.HasPrefix(source, workqueue.Prefix+"receipts/") {
		_, err := s.store.Head(ctx, source)
		if errors.Is(err, storage.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

func diagnosticCollectionReceiptSettled(receipt eventCollectionReceipt) bool {
	if len(receipt.Dispositions) == 0 {
		return receipt.LastEventID == "" && !receipt.Truncated && len(receipt.GapIDs) == 0
	}
	if receipt.SourceVersion == "" || receipt.LastEventID == "" || receipt.Truncated && len(receipt.GapIDs) == 0 {
		return false
	}
	sourceKey := receipt.Dispositions[0].SourceKey
	lastEventID := ""
	for _, disposition := range receipt.Dispositions {
		if disposition.SourceKey == "" || disposition.SourceKey != sourceKey || disposition.SourceVersion != receipt.SourceVersion ||
			disposition.FirstEventID == "" || disposition.LastEventID == "" || disposition.EventCount <= 0 {
			return false
		}
		switch disposition.Disposition {
		case "archived":
			if disposition.BatchID == "" || disposition.BatchSHA256 == "" {
				return false
			}
		case "discarded_by_policy":
			if disposition.GapID == "" || !slicesContains(receipt.GapIDs, disposition.GapID) {
				return false
			}
		default:
			return false
		}
		lastEventID = disposition.LastEventID
	}
	return lastEventID == receipt.LastEventID
}

func (s *Service) classifyTransferObservationEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	switch {
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfers/"),
		strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-sessions/"),
		strings.HasPrefix(entry.Key, diagnostics.Prefix+"upload-receipts/"):
		view, _, err := inventory.Read[transferView](ctx, s.store, entry.Key)
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		if _, terminal := terminalTransferTime(view); !terminal {
			return historyCleanupCandidate{Reason: "active_transfer"}, nil
		}
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-progress/"):
		attempt, _, err := inventory.Read[transferAttempt](ctx, s.store, entry.Key)
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		if attempt.FinishedAt.IsZero() {
			return historyCleanupCandidate{Reason: "active_transfer"}, nil
		}
	}
	if reason := s.transferObservationAgeProtection(entry, cutoff); reason != "" {
		return historyCleanupCandidate{Reason: reason}, nil
	}
	return historyCleanupCandidate{Eligible: true}, nil
}

func (s *Service) transferObservationAgeProtection(entry storage.Entry, cutoff time.Time) string {
	at := time.Unix(0, entry.ModTime)
	if entry.ModTime == 0 || at.After(cutoff) || time.Since(at) < s.diagnosticsConfig().TransferObservationRetention {
		return "minimum_transfer_retention"
	}
	return ""
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
	case "full_work_receipts":
		return "completed work receipts passing minimum age, live-reference, settled-handoff, replay, and execution-fact guards"
	case "diagnostic_collection_receipts":
		return "collection receipts after minimum age and settled source handoff"
	case "transfer_observations":
		return "terminal transfer state and aged transfer session, upload receipt, and progress observations"
	default:
		return "unknown"
	}
}
