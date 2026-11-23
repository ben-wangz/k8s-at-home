package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

const (
	terminalWorkHistoryPrefix       = diagnostics.Prefix + "work-history/"
	terminalWorkHistoryMaxRecords   = 10000
	terminalWorkHistoryMaxBytes     = 64 << 20
	terminalWorkHistoryLegacySource = "work_history_v1_v2"
	terminalWorkHistoryIndexSource  = "work_history_index_v1"
)

type terminalWorkHistoryRecord struct {
	SchemaVersion       int               `json:"schemaVersion"`
	Kind                string            `json:"kind"`
	ID                  string            `json:"id"`
	SourceKey           string            `json:"sourceKey"`
	SourceVersion       string            `json:"sourceVersion"`
	Revision            string            `json:"revision"`
	TerminalAt          time.Time         `json:"terminalAt"`
	UsedModTimeFallback bool              `json:"usedModTimeFallback,omitempty"`
	View                backgroundJobView `json:"view"`
}

func terminalWorkHistoryKey(kind, id string) string {
	return terminalWorkHistoryPrefix + kind + "/" + id + ".json"
}

func (s *Service) projectTerminalWorkHistory(ctx context.Context, sourceKey, sourceVersion string, entry storage.Entry, job workqueue.Job) (bool, error) {
	if !strings.HasPrefix(sourceKey, workqueue.Prefix+"receipts/") || job.State != "done" {
		return true, nil
	}
	terminalAt := job.CompletedAt
	fallback := false
	if terminalAt.IsZero() {
		if entry.ModTime == 0 {
			return false, storage.ErrCorruptObject
		}
		terminalAt, fallback = time.Unix(0, entry.ModTime).UTC(), true
	}
	if historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
		return true, nil
	}
	view := diagnosticJob(job)
	record := terminalWorkHistoryRecord{
		SchemaVersion: 1, Kind: job.Kind, ID: job.ID, SourceKey: sourceKey, SourceVersion: sourceVersion,
		Revision: workqueue.Revision(job), TerminalAt: terminalAt.UTC(), UsedModTimeFallback: fallback, View: view,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	key := terminalWorkHistoryKey(job.Kind, job.ID)
	previous, err := s.reserveTerminalWorkHistory(ctx, key, int64(len(data)))
	if err != nil {
		return false, err
	}
	prior, _, priorErr := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, key)
	if priorErr != nil && !errors.Is(priorErr, storage.ErrNotFound) {
		_ = s.restoreHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, key, int64(len(data)), previous)
		return false, priorErr
	}
	if err := inventory.Update(ctx, s.store, key, func(current *terminalWorkHistoryRecord) error {
		if current.TerminalAt.After(record.TerminalAt) || current.SourceVersion == record.SourceVersion && current.Revision == record.Revision && current.TerminalAt.Equal(record.TerminalAt) {
			return nil
		}
		*current = record
		return nil
	}); err != nil {
		_ = s.restoreHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, key, int64(len(data)), previous)
		return false, err
	}
	indexKey := historyOrderKey(terminalWorkHistoryOrderPrefix, record.TerminalAt, record.Kind, record.ID, record.Revision)
	if err := putHistoryOrderIndex(ctx, s.store, indexKey); err != nil {
		return false, err
	}
	if validTerminalWorkHistory(prior, key) {
		priorIndexKey := historyOrderKey(terminalWorkHistoryOrderPrefix, prior.TerminalAt, prior.Kind, prior.ID, prior.Revision)
		if priorIndexKey != indexKey {
			if err := deleteHistoryOrderIndex(ctx, s.store, priorIndexKey); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func (s *Service) reserveTerminalWorkHistory(ctx context.Context, key string, nextBytes int64) (int64, error) {
	return s.reserveHistoryProjection(ctx, terminalWorkHistoryPrefix, key, nextBytes, terminalWorkHistoryMaxRecords, terminalWorkHistoryMaxBytes)
}

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

func historyRecordExpired(at, now time.Time, retention time.Duration) bool {
	return !at.After(now.UTC().Add(-retention))
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

func scanHistoryEntries(ctx context.Context, store storage.Store, prefix, kind string, maximum int) ([]storage.Entry, int, error) {
	if kind != "" && strings.HasPrefix(prefix, terminalWorkHistoryPrefix) {
		prefix += kind + "/"
	}
	entries := make([]storage.Entry, 0, min(maximum, 256))
	token := ""
	for {
		page, next, err := storage.Scan(ctx, store, prefix, token, min(1000, maximum+1-len(entries)))
		if err != nil {
			return nil, len(entries), err
		}
		entries = append(entries, page...)
		if len(entries) > maximum {
			return nil, len(entries), diagnostics.ErrHistoryCapacity
		}
		if next == "" {
			return entries, len(entries), nil
		}
		token = next
	}
}

func historyKindFilter(values url.Values) string { return values.Get("kind") }

func terminalWorkHistorySource(projectionReadsEnabled bool) string {
	if projectionReadsEnabled {
		return terminalWorkHistoryIndexSource
	}
	return terminalWorkHistoryLegacySource
}

func (s *Service) terminalWorkHistoryDetail(ctx context.Context, kind, id string) (backgroundJobView, error) {
	key := terminalWorkHistoryKey(kind, id)
	receiptKey := workqueue.ReceiptKey(kind, id)
	entry, headErr := s.store.Head(ctx, receiptKey)
	if headErr != nil && !errors.Is(headErr, storage.ErrNotFound) {
		return backgroundJobView{}, headErr
	}
	record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, key)
	if err == nil {
		if !validTerminalWorkHistory(record, key) {
			return backgroundJobView{}, storage.ErrCorruptObject
		}
		if errors.Is(headErr, storage.ErrNotFound) || record.SourceVersion == sourceEntryVersion(entry) {
			if historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
				return backgroundJobView{}, inventory.ErrExpired
			}
			return record.View, nil
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return backgroundJobView{}, err
	}
	receipt, version, receiptErr := inventory.Read[workqueue.Job](ctx, s.store, receiptKey)
	if receiptErr == nil {
		if receipt.ID != id || receipt.Kind != kind || receipt.State != "done" {
			return backgroundJobView{}, storage.ErrCorruptObject
		}
		terminalAt := receipt.CompletedAt
		if terminalAt.IsZero() && entry.ModTime != 0 {
			terminalAt = time.Unix(0, entry.ModTime).UTC()
		}
		if terminalAt.IsZero() {
			return backgroundJobView{}, inventory.ErrExpired
		}
		if historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			return backgroundJobView{}, inventory.ErrExpired
		}
		if s.cfg.Diagnostics.HistoryProjectionReadsEnabled {
			return backgroundJobView{}, storage.ErrUnavailable
		}
		receipt.Version = version
		return diagnosticJob(receipt), nil
	}
	if !errors.Is(receiptErr, storage.ErrNotFound) {
		return backgroundJobView{}, receiptErr
	}
	marker, _, markerErr := inventory.Read[workqueue.CompletionMarker](ctx, s.store, workqueue.CompletionMarkerKey(kind, id))
	if markerErr == nil {
		if marker.SchemaVersion != 1 || marker.Kind != kind || marker.ID != id || marker.Disposition != "done" {
			return backgroundJobView{}, storage.ErrCorruptObject
		}
		if marker.CompletedAt.IsZero() || historyRecordExpired(marker.CompletedAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			return backgroundJobView{}, inventory.ErrExpired
		}
		return backgroundJobView{}, storage.ErrUnavailable
	}
	if !errors.Is(markerErr, storage.ErrNotFound) {
		return backgroundJobView{}, markerErr
	}
	return backgroundJobView{}, storage.ErrNotFound
}
