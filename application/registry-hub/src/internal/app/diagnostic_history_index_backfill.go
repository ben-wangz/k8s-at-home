package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

const (
	terminalWorkHistoryRetentionWorker     = "diagnostic_retention_work_history"
	terminalTransferHistoryRetentionWorker = "diagnostic_retention_transfer_history"
	terminalHistoryIndexBackfillVersion    = 1
)

func terminalHistoryIndexBackfillWorker(kind string) bool {
	return kind == terminalWorkHistoryRetentionWorker || kind == terminalTransferHistoryRetentionWorker
}

func prepareTerminalHistoryIndexBackfill(kind string, p *sourceSweepCheckpoint) {
	if !terminalHistoryIndexBackfillWorker(kind) || p.HistoryIndexBackfillVersion == terminalHistoryIndexBackfillVersion {
		return
	}
	p.HistoryIndexBackfillVersion = terminalHistoryIndexBackfillVersion
	p.HistoryIndexBackfillComplete = false
	p.Cursor = inventory.Cursor{}
	p.NextRound = time.Time{}
	p.IdleRounds = 0
	p.RoundFailed = false
}

func (s *Service) sweepTerminalHistory(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	if p.HistoryIndexBackfillComplete {
		if _, retry := ctx.Value(terminalHistoryRetryKey{}).(bool); !retry {
			kind := terminalTransferHistoryRetentionWorker
			if strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix) {
				kind = terminalWorkHistoryRetentionWorker
			}
			ctx = context.WithValue(ctx, terminalHistoryRetentionWorkerKey{}, kind)
		}
		return s.cleanupDiagnosticRetentionEntry(ctx, entry, p)
	}
	indexKey, err := s.readTerminalHistoryIndexKey(ctx, entry.Key)
	if err != nil {
		return err
	}
	if _, err = s.store.Head(ctx, indexKey); !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := putHistoryOrderIndex(ctx, s.store, indexKey); err != nil {
		return err
	}
	incrementSweepCount(p, "indexes_backfilled", 1)
	return nil
}

func (s *Service) finishTerminalHistoryIndexBackfill(kind string) sourceSweepEnd {
	return func(ctx context.Context, p *sourceSweepCheckpoint) error {
		if p.HistoryIndexBackfillComplete || p.RoundFailed || len(p.Backoff) > 0 {
			return nil
		}
		// Pending retries can be newer than the last published scan status.
		retries, next, err := storage.Scan(ctx, s.store, sourceRetryPrefix+kind+"/", "", 1)
		if err != nil {
			return err
		}
		p.HistoryIndexBackfillComplete = len(retries) == 0 && next == ""
		return nil
	}
}

func (s *Service) readTerminalHistoryIndexKey(ctx context.Context, key string) (string, error) {
	if strings.HasPrefix(key, terminalWorkHistoryPrefix) {
		record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, key)
		if err != nil {
			return "", err
		}
		if !validTerminalWorkHistory(record, key) {
			return "", storage.ErrCorruptObject
		}
		return historyOrderKey(terminalWorkHistoryOrderPrefix, record.TerminalAt, record.Kind, record.ID, record.Revision), nil
	}
	record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, key)
	if err != nil {
		return "", err
	}
	if !validTerminalTransferHistory(record, key) || record.View.Kind == "" {
		return "", storage.ErrCorruptObject
	}
	return historyOrderKey(terminalTransferHistoryOrderPrefix, record.TerminalAt, record.View.Kind, record.ID, record.Revision), nil
}

func (s *Service) terminalHistoryIndexLinked(ctx context.Context, prefix, indexKey string) (bool, error) {
	kind, id, ok := parseHistoryOrderKey(prefix, indexKey)
	if !ok {
		return false, nil
	}
	projectionKey := terminalTransferHistoryKey(id)
	if prefix == terminalWorkHistoryOrderPrefix {
		projectionKey = terminalWorkHistoryKey(kind, id)
	}
	expected, err := s.readTerminalHistoryIndexKey(ctx, projectionKey)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return expected == indexKey, err
}
