package app

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) listTerminalTransferHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int) ([]terminalTransferHistoryRecord, int, bool, error) {
	if cursor.HistorySource == "transfer_history_index_v1" {
		return s.listIndexedTerminalTransferHistory(ctx, values, cursor, limit)
	}
	if cursor.HistoryBefore.IsZero() {
		cursor.HistoryBefore = time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	rows := map[string]terminalTransferHistoryRecord{}
	entries, scanned, err := scanHistoryEntries(ctx, s.store, terminalTransferHistoryPrefix, "", terminalTransferHistoryMaxRows)
	if err != nil {
		return nil, scanned, false, err
	}
	for _, entry := range entries {
		record, _, readErr := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, entry.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, scanned, false, readErr
		}
		if validTerminalTransferHistory(record, entry.Key) && record.TerminalAt.After(cursor.HistoryBefore) {
			rows[record.ID] = record
		}
	}
	baseEntries, baseScanned, err := scanHistoryEntries(ctx, s.store, diagnostics.Prefix+"transfers/", "", terminalTransferHistoryMaxRows)
	if err != nil {
		return nil, scanned + baseScanned, false, err
	}
	scanned += baseScanned
	for _, entry := range baseEntries {
		view, _, readErr := inventory.Read[transferView](ctx, s.store, entry.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, scanned, false, readErr
		}
		terminalAt, terminal := terminalTransferTime(view)
		if !terminal || !terminalAt.After(cursor.HistoryBefore) {
			continue
		}
		if projected, found := rows[view.ID]; found && projected.SourceVersion == sourceEntryVersion(entry) {
			continue
		}
		view.Attempt = nil
		view.ObservedAt = terminalAt
		rows[view.ID] = terminalTransferHistoryRecord{SchemaVersion: 1, ID: view.ID, SourceKey: entry.Key, SourceVersion: sourceEntryVersion(entry), Revision: diagnostics.ID("transfer-history", view.ID, view.TransferState, terminalAt.Format(time.RFC3339Nano), view.ProgressAt.Format(time.RFC3339Nano)), TerminalAt: terminalAt, View: view}
	}
	ordered := make([]terminalTransferHistoryRecord, 0, len(rows))
	for _, record := range rows {
		if transferHistoryMatches(record.View, values) && historyAfterCursor(terminalWorkHistoryRecord{Kind: record.View.Kind, ID: record.ID, Revision: record.Revision, TerminalAt: record.TerminalAt}, *cursor) {
			ordered = append(ordered, record)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return terminalTransferHistoryLess(ordered[i], ordered[j]) })
	more := len(ordered) > limit
	if more {
		ordered = ordered[:limit]
	}
	return ordered, scanned, more, nil
}

func (s *Service) listIndexedTerminalTransferHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int) ([]terminalTransferHistoryRecord, int, bool, error) {
	if limit <= 0 {
		return nil, 0, false, nil
	}
	entries, next, err := storage.Scan(ctx, s.store, terminalTransferHistoryOrderPrefix, cursor.HistoryIndexToken, limit)
	if err != nil {
		return nil, 0, false, err
	}
	rows := make([]terminalTransferHistoryRecord, 0, len(entries))
	for _, entry := range entries {
		_, id, ok := parseHistoryOrderKey(terminalTransferHistoryOrderPrefix, entry.Key)
		if !ok {
			continue
		}
		key := terminalTransferHistoryKey(id)
		record, _, readErr := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, len(entries), false, readErr
		}
		if !validTerminalTransferHistory(record, key) || historyOrderKey(terminalTransferHistoryOrderPrefix, record.TerminalAt, record.View.Kind, record.ID, record.Revision) != entry.Key ||
			!record.TerminalAt.After(cursor.HistoryBefore) || !transferHistoryMatches(record.View, values) {
			continue
		}
		rows = append(rows, record)
	}
	cursor.HistoryIndexToken = next
	return rows, len(entries), next != "", nil
}

func validTerminalTransferHistory(record terminalTransferHistoryRecord, key string) bool {
	return record.SchemaVersion == 1 && record.ID != "" && record.SourceKey == diagnostics.Prefix+"transfers/"+record.ID+".json" &&
		record.SourceVersion != "" && record.Revision != "" && !record.TerminalAt.IsZero() &&
		key == terminalTransferHistoryKey(record.ID) && record.View.ID == record.ID
}

func transferHistoryMatches(view transferView, values url.Values) bool {
	fields := map[string]string{"kind": view.Kind, "state": view.TransferState, "authority": view.Authority, "repository": view.Repository, "digest": view.Digest, "taskID": view.TaskID, "intentID": view.IntentID, "sessionID": view.SessionID, "cleanupState": view.CleanupState, "errorClass": view.ErrorClass}
	for key, value := range fields {
		if values.Get(key) != "" && values.Get(key) != value {
			return false
		}
	}
	return true
}

func terminalTransferHistoryLess(left, right terminalTransferHistoryRecord) bool {
	if !left.TerminalAt.Equal(right.TerminalAt) {
		return left.TerminalAt.After(right.TerminalAt)
	}
	if left.View.Kind != right.View.Kind {
		return left.View.Kind < right.View.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	return left.Revision < right.Revision
}
