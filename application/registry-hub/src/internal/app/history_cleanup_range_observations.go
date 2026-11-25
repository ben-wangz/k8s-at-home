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

func (s *Service) classifyHistoryCleanupFullReceipt(ctx context.Context, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	_, id, ok := receiptWorkIdentity(entry.Key)
	if !ok {
		return historyCleanupRangeCandidate{Reason: "invalid_receipt_key"}, true, nil
	}
	receipt, _, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if receipt.ID != id || receipt.State != "done" {
		return historyCleanupRangeCandidate{Reason: "active_receipt"}, true, nil
	}
	at := receipt.CompletedAt
	if at.IsZero() && entry.ModTime != 0 {
		at = time.Unix(0, entry.ModTime).UTC()
	}
	if !historyCleanupTimeInRange(at, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	candidate, err := s.classifyWorkReceiptEntry(ctx, entry, scope.To)
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if !candidate.Eligible {
		return historyCleanupRangeCandidate{Reason: candidate.Reason}, true, nil
	}
	return eligibleHistoryCleanupRangeEntry(entry, at)
}

func (s *Service) classifyHistoryCleanupCollectionReceipt(ctx context.Context, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	at := receipt.UpdatedAt
	if at.IsZero() && entry.ModTime != 0 {
		at = time.Unix(0, entry.ModTime).UTC()
	}
	if !historyCleanupTimeInRange(at, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	safe, err := s.diagnosticCollectionReceiptRemovable(ctx, entry.Key, receipt)
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if !safe {
		return historyCleanupRangeCandidate{Reason: "handoff_pending"}, true, nil
	}
	if time.Since(at) < s.diagnosticsConfig().CollectedReceiptRetention {
		return historyCleanupRangeCandidate{Reason: "minimum_collection_retention"}, true, nil
	}
	return eligibleHistoryCleanupRangeEntry(entry, at)
}

func (s *Service) classifyHistoryCleanupTransferObservation(ctx context.Context, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	at := time.Time{}
	if entry.ModTime != 0 {
		at = time.Unix(0, entry.ModTime).UTC()
	}
	if strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfers/") {
		view, _, err := inventory.Read[transferView](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return historyCleanupRangeCandidate{}, false, nil
		}
		if err != nil {
			return historyCleanupRangeCandidate{}, false, err
		}
		terminalAt, terminal := terminalTransferTime(view)
		if terminal {
			at = terminalAt
		} else {
			for _, observed := range []time.Time{view.StartedAt, view.ProgressAt, view.ObservedAt} {
				if observed.After(at) {
					at = observed
				}
			}
		}
		if !historyCleanupTimeInRange(at, scope) {
			return historyCleanupRangeCandidate{}, false, nil
		}
		if !terminal {
			return historyCleanupRangeCandidate{Reason: "active_transfer"}, true, nil
		}
		if reason := s.transferObservationAgeProtection(entry, time.Now().UTC()); reason != "" {
			return historyCleanupRangeCandidate{Reason: reason}, true, nil
		}
		return eligibleHistoryCleanupRangeEntry(entry, at)
	}
	if !historyCleanupTimeInRange(at, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	candidate, err := s.classifyTransferObservationEntry(ctx, entry, time.Now().UTC())
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if !candidate.Eligible {
		return historyCleanupRangeCandidate{Reason: candidate.Reason}, true, nil
	}
	return eligibleHistoryCleanupRangeEntry(entry, at)
}

func eligibleHistoryCleanupRangeEntry(entry storage.Entry, at time.Time) (historyCleanupRangeCandidate, bool, error) {
	ref := historyCleanupRef(entry)
	ref.At = at
	return historyCleanupRangeCandidate{Eligible: true, Reference: ref}, true, nil
}

func historyCleanupRef(entry storage.Entry) historyCleanupCandidateRef {
	return historyCleanupCandidateRef{Key: entry.Key, Version: entry.Version, Fingerprint: entry.MetadataFingerprint, ModTime: entry.ModTime, Size: entry.Size}
}

func historyCleanupTimeInRange(at time.Time, scope historyCleanupRange) bool {
	return !at.IsZero() && !at.Before(scope.From) && at.Before(scope.To)
}
