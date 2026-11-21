package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const (
	sourceRetryStatusRefreshInterval   = 5 * time.Second
	sourceRetryStatusRefreshMaxBackoff = time.Minute
	sourceRetryStatusReadBudget        = 5 * time.Second
)

type sourceRetryStatusRefreshState struct {
	nextAt  time.Time
	backoff time.Duration
}

func (s *Service) refreshSourceRetryStatusIfDue(ctx context.Context, holder lease.Lease, kind string, p *sourceSweepCheckpoint, state *sourceRetryStatusRefreshState) error {
	if !sourceSweepUsesDurableRetry(kind) || time.Now().Before(state.nextAt) {
		return nil
	}
	bounded, release, admitted := s.beginSourceSweepUnit(ctx, kind)
	if !admitted {
		state.deferNext(time.Now())
		return nil
	}
	statusCtx, cancel := context.WithTimeout(bounded, sourceRetryStatusReadBudget)
	statusCtx = context.WithValue(statusCtx, diagnosticContextKey{}, backgroundDiagnosticContext{
		Kind: kind, RunID: p.Status.RunID, Scope: "retry_status_refresh", Budget: sourceRetryStatusReadBudget.Seconds(),
	})
	previous := p.Status
	err := s.updateSourceRetryStatus(statusCtx, kind, p)
	cancel()
	release()
	completedAt := time.Now()
	if err != nil {
		state.deferNext(completedAt)
		if ctx.Err() == nil {
			s.backgroundEvent(kind, "retry_status_refresh_failed", err)
		}
		return nil
	}
	markSourceRetryRoundSuccess(p)
	state.succeeded(completedAt)
	if !sourceRetryStatusChanged(previous, p.Status) {
		return nil
	}
	checkpointCtx, cancelCheckpoint := s.backgroundCheckpoint(ctx, kind)
	p.Status.CheckpointAt = time.Now().UTC()
	err = s.saveSourceSweep(checkpointCtx, holder, kind, *p)
	cancelCheckpoint()
	return err
}

func (s *sourceRetryStatusRefreshState) deferNext(now time.Time) {
	if s.backoff <= 0 {
		s.backoff = sourceRetryStatusRefreshInterval
	}
	s.nextAt = now.Add(s.backoff)
	s.backoff = min(s.backoff*2, sourceRetryStatusRefreshMaxBackoff)
}

func (s *sourceRetryStatusRefreshState) succeeded(now time.Time) {
	s.nextAt = now.Add(sourceRetryStatusRefreshInterval)
	s.backoff = sourceRetryStatusRefreshInterval
}

func sourceRetryStatusChanged(previous, next backgroundScanStatus) bool {
	return previous.PendingRetries != next.PendingRetries ||
		!previous.OldestDueRetryAt.Equal(next.OldestDueRetryAt) ||
		!previous.NextRetryAt.Equal(next.NextRetryAt) ||
		previous.LastSuccessfulRound != next.LastSuccessfulRound ||
		!previous.LastSuccessAt.Equal(next.LastSuccessAt) ||
		previous.ErrorRecovered != next.ErrorRecovered ||
		!previous.NextAttemptAt.Equal(next.NextAttemptAt)
}

func (s *Service) updateSourceRetryStatus(ctx context.Context, kind string, p *sourceSweepCheckpoint) error {
	var pendingRetries int64
	token := ""
	for {
		entries, next, err := s.store.List(ctx, sourceRetryPrefix+kind+"/", token, 1000)
		if err != nil {
			return err
		}
		pendingRetries += int64(len(entries))
		if next == "" {
			break
		}
		if next == token {
			return storage.ErrUnavailable
		}
		token = next
	}
	dueEntries, _, err := s.store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 1000)
	if err != nil {
		return err
	}
	var oldestDueRetryAt, nextRetryAt time.Time
	now := time.Now().UTC()
	for _, entry := range dueEntries {
		index, _, err := inventory.Read[sourceSweepRetryDue](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		retry, _, err := inventory.Read[sourceSweepRetry](ctx, s.store, sourceRetryRecordKey(kind, index.RetryID))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if retry.State != "pending" || retry.Revision != index.Revision || retry.NextAttemptAt != index.DueAt {
			continue
		}
		nextRetryAt = retry.NextAttemptAt
		if !retry.NextAttemptAt.After(now) {
			oldestDueRetryAt = retry.NextAttemptAt
		}
		break
	}
	p.Status.PendingRetries = pendingRetries
	p.Status.OldestDueRetryAt = oldestDueRetryAt
	p.Status.NextRetryAt = nextRetryAt
	return nil
}
