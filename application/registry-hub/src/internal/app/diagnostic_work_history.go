package app

import (
	"context"
	"encoding/json"
	"errors"
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

func historyRecordExpired(at, now time.Time, retention time.Duration) bool {
	return !at.After(now.UTC().Add(-retention))
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
