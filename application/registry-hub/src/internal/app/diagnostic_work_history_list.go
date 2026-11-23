package app

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func (s *Service) listTerminalWorkHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int) ([]terminalWorkHistoryRecord, int, bool, bool, error) {
	if cursor.HistorySource == "work_history_index_v1" {
		return s.listIndexedTerminalWorkHistory(ctx, values, cursor, limit)
	}
	if cursor.HistoryBefore.IsZero() {
		cursor.HistoryBefore = time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	rows := map[string]terminalWorkHistoryRecord{}
	entries, scanned, err := scanHistoryEntries(ctx, s.store, terminalWorkHistoryPrefix, historyKindFilter(values), terminalWorkHistoryMaxRecords)
	if err != nil {
		return nil, scanned, false, false, err
	}
	for _, entry := range entries {
		record, _, readErr := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, entry.Key)
		if readErr != nil {
			if errors.Is(readErr, storage.ErrNotFound) {
				continue
			}
			return nil, scanned, false, false, readErr
		}
		if !validTerminalWorkHistory(record, entry.Key) || !record.TerminalAt.After(cursor.HistoryBefore) {
			continue
		}
		rows[record.Kind+"/"+record.ID] = record
	}
	receiptPrefix := workqueue.Prefix + "receipts/"
	if kind := historyKindFilter(values); kind != "" {
		receiptPrefix += kind + "/"
	}
	receipts, receiptScanned, err := scanHistoryEntries(ctx, s.store, receiptPrefix, "", terminalWorkHistoryMaxRecords)
	if err != nil {
		return nil, scanned + receiptScanned, false, false, err
	}
	scanned += receiptScanned
	projectionByKey := make(map[string]terminalWorkHistoryRecord, len(rows))
	for _, record := range rows {
		projectionByKey[record.SourceKey] = record
	}
	usedFallback := false
	for _, entry := range receipts {
		if record, found := projectionByKey[entry.Key]; found && record.SourceVersion == sourceEntryVersion(entry) {
			continue
		}
		job, version, readErr := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, scanned, false, false, readErr
		}
		if job.State != "done" || job.ID == "" || job.Kind == "" {
			continue
		}
		terminalAt := job.CompletedAt
		if terminalAt.IsZero() && entry.ModTime != 0 {
			terminalAt = time.Unix(0, entry.ModTime).UTC()
		}
		if terminalAt.IsZero() || !terminalAt.After(cursor.HistoryBefore) {
			continue
		}
		job.Version = version
		record := terminalWorkHistoryRecord{SchemaVersion: 1, Kind: job.Kind, ID: job.ID, SourceKey: entry.Key, SourceVersion: sourceEntryVersion(entry), Revision: workqueue.Revision(job), TerminalAt: terminalAt.UTC(), UsedModTimeFallback: job.CompletedAt.IsZero(), View: diagnosticJob(job)}
		rows[job.Kind+"/"+job.ID] = record
		usedFallback = true
	}
	ordered := make([]terminalWorkHistoryRecord, 0, len(rows))
	for _, record := range rows {
		if matchesWork(record.View, values) && historyAfterCursor(record, *cursor) {
			ordered = append(ordered, record)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return terminalWorkHistoryLess(ordered[i], ordered[j]) })
	more := len(ordered) > limit
	if more {
		ordered = ordered[:limit]
	}
	return ordered, scanned, more, usedFallback, nil
}

func (s *Service) listIndexedTerminalWorkHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int) ([]terminalWorkHistoryRecord, int, bool, bool, error) {
	if limit <= 0 {
		return nil, 0, false, false, nil
	}
	entries, next, err := storage.Scan(ctx, s.store, terminalWorkHistoryOrderPrefix, cursor.HistoryIndexToken, limit)
	if err != nil {
		return nil, 0, false, false, err
	}
	rows := make([]terminalWorkHistoryRecord, 0, len(entries))
	for _, entry := range entries {
		kind, id, ok := parseHistoryOrderKey(terminalWorkHistoryOrderPrefix, entry.Key)
		if !ok || historyKindFilter(values) != "" && historyKindFilter(values) != kind {
			continue
		}
		key := terminalWorkHistoryKey(kind, id)
		record, _, readErr := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, len(entries), false, false, readErr
		}
		if !validTerminalWorkHistory(record, key) || historyOrderKey(terminalWorkHistoryOrderPrefix, record.TerminalAt, record.Kind, record.ID, record.Revision) != entry.Key ||
			!record.TerminalAt.After(cursor.HistoryBefore) || !matchesWork(record.View, values) {
			continue
		}
		rows = append(rows, record)
	}
	cursor.HistoryIndexToken = next
	return rows, len(entries), next != "", false, nil
}

func validTerminalWorkHistory(record terminalWorkHistoryRecord, key string) bool {
	return record.SchemaVersion == 1 && record.Kind != "" && record.ID != "" && record.SourceVersion != "" &&
		record.Revision != "" && !record.TerminalAt.IsZero() && key == terminalWorkHistoryKey(record.Kind, record.ID) &&
		record.View.ID == record.ID && record.View.Kind == record.Kind
}

func historyAfterCursor(record terminalWorkHistoryRecord, cursor diagnosticCursor) bool {
	if cursor.HistoryAfter.IsZero() {
		return true
	}
	last := terminalWorkHistoryRecord{TerminalAt: cursor.HistoryAfter, Kind: cursor.HistoryKind, ID: cursor.HistoryID, Revision: cursor.HistoryRevision}
	return terminalWorkHistoryLess(last, record)
}

func terminalWorkHistoryLess(left, right terminalWorkHistoryRecord) bool {
	if !left.TerminalAt.Equal(right.TerminalAt) {
		return left.TerminalAt.After(right.TerminalAt)
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	return left.Revision < right.Revision
}

func historyKindFilter(values url.Values) string {
	return values.Get("kind")
}
