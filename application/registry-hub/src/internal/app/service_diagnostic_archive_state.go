package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func diagnosticOutboxSettled(source, version string, events []diagnostics.Event, receipt eventCollectionReceipt) bool {
	if len(events) == 0 {
		return true
	}
	if receipt.SourceVersion != version || len(receipt.Dispositions) == 0 || receipt.LastEventID != events[len(events)-1].ID {
		return false
	}
	if receipt.Truncated && len(receipt.GapIDs) == 0 {
		return false
	}
	offset := 0
	for _, disposition := range receipt.Dispositions {
		if disposition.SourceKey != source || disposition.SourceVersion != version || disposition.EventCount < 1 ||
			disposition.Disposition != "archived" && disposition.Disposition != "discarded_by_policy" || offset+disposition.EventCount > len(events) ||
			disposition.FirstEventID != events[offset].ID {
			return false
		}
		end := offset + disposition.EventCount - 1
		if disposition.LastEventID != events[end].ID {
			return false
		}
		if disposition.Disposition == "archived" && (disposition.BatchID == "" || disposition.BatchSHA256 == "") ||
			disposition.Disposition == "discarded_by_policy" && disposition.GapID == "" {
			return false
		}
		offset += disposition.EventCount
	}
	return offset == len(events)
}

func (s *Service) saveDiagnosticReceiptArchive(ctx context.Context, key string, state diagnosticReceiptArchive, version string) error {
	state.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: version == ""}
	if version != "" {
		options = storage.PutOptions{IfMatch: version}
	}
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, _, readErr := inventory.Read[diagnosticReceiptArchive](ctx, s.store, key)
		if readErr == nil && stored.SourceKey == state.SourceKey && stored.SourceVersion == state.SourceVersion && stored.SourceFingerprint == state.SourceFingerprint &&
			stored.ProjectionVersion == state.ProjectionVersion && stored.LastArchivedEventID == state.LastArchivedEventID &&
			stored.NotificationIndexed == state.NotificationIndexed && stored.TransferProjected == state.TransferProjected &&
			stored.ExecutionFactProjected == state.ExecutionFactProjected && stored.ExecutionSourcesVerified == state.ExecutionSourcesVerified &&
			stored.ExecutionVerificationDone == state.ExecutionVerificationDone && stored.HistoryProjected == state.HistoryProjected &&
			stored.OutboxComplete == state.OutboxComplete && stored.Complete == state.Complete {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}
