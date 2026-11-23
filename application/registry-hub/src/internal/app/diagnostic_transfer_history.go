package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

const (
	terminalTransferHistoryPrefix       = diagnostics.Prefix + "transfer-history/"
	terminalTransferExpiredPrefix       = diagnostics.Prefix + "transfer-history-expired/"
	terminalTransferHistoryMaxRows      = 10000
	terminalTransferHistoryMaxBytes     = int64(64 << 20)
	terminalTransferHistoryLegacySource = "transfer_history_v1_v2"
	terminalTransferHistoryIndexSource  = "transfer_history_index_v1"
)

type terminalTransferHistoryRecord struct {
	SchemaVersion int          `json:"schemaVersion"`
	ID            string       `json:"id"`
	SourceKey     string       `json:"sourceKey"`
	SourceVersion string       `json:"sourceVersion"`
	Revision      string       `json:"revision"`
	TerminalAt    time.Time    `json:"terminalAt"`
	View          transferView `json:"view"`
}

type terminalTransferExpiry struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	TerminalAt    time.Time `json:"terminalAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

func terminalTransferHistoryKey(id string) string {
	return terminalTransferHistoryPrefix + id + ".json"
}

func terminalTransferExpiryKey(id string) string { return terminalTransferExpiredPrefix + id + ".json" }

func terminalTransferHistorySource(projectionReadsEnabled bool) string {
	if projectionReadsEnabled {
		return terminalTransferHistoryIndexSource
	}
	return terminalTransferHistoryLegacySource
}

func terminalTransferTime(view transferView) (time.Time, bool) {
	terminal := false
	switch view.Kind {
	case "client_upload":
		terminal = view.TransferState == "complete" || view.TransferState == "cancelled"
	case "distribution_write":
		terminal = view.TransferState == "solidified" || view.TransferState == "failed" || view.TransferState == "cancelled"
	case "upstream_download":
		terminal = view.CacheCommitState == "committed" || view.CleanupState == "completed_or_superseded"
	}
	if !terminal {
		return time.Time{}, false
	}
	at := view.FinishedAt
	if at.IsZero() {
		at = view.ProgressAt
	}
	if at.IsZero() {
		at = view.ObservedAt
	}
	return at.UTC(), !at.IsZero()
}

func (s *Service) projectTerminalTransferHistory(ctx context.Context, view transferView, expectedSourceVersion string) error {
	terminalAt, terminal := terminalTransferTime(view)
	if !terminal {
		return nil
	}
	key := diagnostics.Prefix + "transfers/" + view.ID + ".json"
	entry, err := s.store.Head(ctx, key)
	if err != nil {
		return err
	}
	version := sourceEntryVersion(entry)
	if expectedSourceVersion != "" && version != expectedSourceVersion {
		return storage.ErrConditionFailed
	}
	if err := inventory.Update(ctx, s.store, terminalTransferExpiryKey(view.ID), func(current *terminalTransferExpiry) error {
		if current.TerminalAt.After(terminalAt) {
			return nil
		}
		*current = terminalTransferExpiry{SchemaVersion: 1, ID: view.ID, TerminalAt: terminalAt, ExpiresAt: terminalAt.Add(s.diagnosticsConfig().TransferExpiryRetention)}
		return nil
	}); err != nil {
		return err
	}
	if historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
		return nil
	}
	view.Attempt = nil
	view.ObservedAt = terminalAt
	record := terminalTransferHistoryRecord{
		SchemaVersion: 1, ID: view.ID, SourceKey: key, SourceVersion: version,
		Revision:   diagnostics.ID("transfer-history", view.ID, view.TransferState, terminalAt.Format(time.RFC3339Nano), view.ProgressAt.Format(time.RFC3339Nano)),
		TerminalAt: terminalAt, View: view,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	projectionKey := terminalTransferHistoryKey(view.ID)
	previous, err := s.reserveHistoryProjection(ctx, terminalTransferHistoryPrefix, projectionKey, int64(len(data)), terminalTransferHistoryMaxRows, terminalTransferHistoryMaxBytes)
	if err != nil {
		return err
	}
	currentSource, err := s.store.Head(ctx, key)
	if err != nil || sourceEntryVersion(currentSource) != version {
		_ = s.restoreHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, projectionKey, int64(len(data)), previous)
		if err != nil {
			return err
		}
		return storage.ErrConditionFailed
	}
	prior, _, priorErr := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, projectionKey)
	if priorErr != nil && !errors.Is(priorErr, storage.ErrNotFound) {
		_ = s.restoreHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, projectionKey, int64(len(data)), previous)
		return priorErr
	}
	if err := inventory.Update(ctx, s.store, projectionKey, func(current *terminalTransferHistoryRecord) error {
		if current.TerminalAt.After(record.TerminalAt) || current.SourceVersion == record.SourceVersion && current.Revision == record.Revision {
			return nil
		}
		*current = record
		return nil
	}); err != nil {
		_ = s.restoreHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, projectionKey, int64(len(data)), previous)
		return err
	}
	indexKey := historyOrderKey(terminalTransferHistoryOrderPrefix, record.TerminalAt, record.View.Kind, record.ID, record.Revision)
	if err := putHistoryOrderIndex(ctx, s.store, indexKey); err != nil {
		return err
	}
	if validTerminalTransferHistory(prior, projectionKey) {
		priorIndexKey := historyOrderKey(terminalTransferHistoryOrderPrefix, prior.TerminalAt, prior.View.Kind, prior.ID, prior.Revision)
		if priorIndexKey != indexKey {
			if err := deleteHistoryOrderIndex(ctx, s.store, priorIndexKey); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) terminalTransferHistoryDetail(ctx context.Context, id string) (transferView, error) {
	baseKey := diagnostics.Prefix + "transfers/" + id + ".json"
	base, _, baseErr := inventory.Read[transferView](ctx, s.store, baseKey)
	if baseErr != nil && !errors.Is(baseErr, storage.ErrNotFound) {
		return transferView{}, baseErr
	}
	if baseErr == nil {
		entry, headErr := s.store.Head(ctx, baseKey)
		if headErr != nil {
			return transferView{}, headErr
		}
		if record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, terminalTransferHistoryKey(id)); err == nil &&
			validTerminalTransferHistory(record, terminalTransferHistoryKey(id)) && record.SourceVersion == sourceEntryVersion(entry) {
			if historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
				return transferView{}, inventory.ErrExpired
			}
			return record.View, nil
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return transferView{}, err
		}
		if s.cfg.Diagnostics.HistoryProjectionReadsEnabled {
			if terminalAt, terminal := terminalTransferTime(base); terminal {
				if historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
					return transferView{}, inventory.ErrExpired
				}
				return transferView{}, storage.ErrUnavailable
			}
			return base, nil
		}
	}
	record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, terminalTransferHistoryKey(id))
	if err == nil && validTerminalTransferHistory(record, terminalTransferHistoryKey(id)) {
		if errors.Is(baseErr, storage.ErrNotFound) {
			if historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
				return transferView{}, inventory.ErrExpired
			}
			return record.View, nil
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return transferView{}, err
	}
	if baseErr == nil {
		if terminalAt, terminal := terminalTransferTime(base); terminal && historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			return transferView{}, inventory.ErrExpired
		}
		if s.cfg.Diagnostics.HistoryProjectionReadsEnabled {
			if _, terminal := terminalTransferTime(base); terminal {
				return transferView{}, storage.ErrUnavailable
			}
		}
		return base, nil
	}
	expiry, _, expiryErr := inventory.Read[terminalTransferExpiry](ctx, s.store, terminalTransferExpiryKey(id))
	if expiryErr == nil && expiry.SchemaVersion == 1 && expiry.ID == id && !expiry.TerminalAt.IsZero() && expiry.ExpiresAt.After(time.Now().UTC()) {
		return transferView{}, inventory.ErrExpired
	}
	if expiryErr != nil && !errors.Is(expiryErr, storage.ErrNotFound) {
		return transferView{}, expiryErr
	}
	return transferView{}, storage.ErrNotFound
}
