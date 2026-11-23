package app

import (
	"context"
	"registry-hub/internal/inventory"
	"strings"
	"time"
)

func (s *Service) initializeDiagnosticEventRound(ctx context.Context, prefix string, p *sourceSweepCheckpoint, startedAt time.Time) error {
	p.EventRoundEndDate = startedAt.Add(-s.diagnosticsConfig().EventRetention).Format("20060102")
	entries, _, err := s.store.List(ctx, prefix, "", 1)
	if err != nil {
		p.EventRoundEndDate = ""
		return err
	}
	if len(entries) == 0 {
		p.EventPartitionDate = "none"
		return nil
	}
	date := strings.TrimPrefix(entries[0].Key, prefix)
	if slash := strings.IndexByte(date, '/'); slash >= 0 {
		date = date[:slash]
	}
	if _, err := time.Parse("20060102", date); err != nil || date > p.EventRoundEndDate {
		p.EventPartitionDate = "none"
	} else {
		p.EventPartitionDate = date
	}
	return nil
}

func (s *Service) sourceSweepBatch(ctx context.Context, kind, prefix string, p *sourceSweepCheckpoint, step sourceSweepStep) {
	limit := 1
	if kind == "quota_waiters" {
		limit = 16
	} else if sourceSweepUsesDurableRetry(kind) {
		limit = 8
	}
	for unit := 0; unit < limit && sourceBudgetAvailable(ctx); unit++ {
		if !s.sourceSweepEntity(ctx, kind, prefix, p, step) {
			break
		}
	}
}

func (s *Service) completeSourceRound(kind string, p *sourceSweepCheckpoint) {
	completedRound := p.Round
	for id, b := range p.Backoff {
		if b.Round != p.Round {
			delete(p.Backoff, id)
		}
	}
	status := &p.Status
	status.Complete, status.ObservedAt = len(p.Backoff) == 0 && !p.RoundFailed, time.Now().UTC()
	status.LastScanCompletedAt = status.ObservedAt
	s.metrics.recordBackgroundScanRound(kind, status.Complete)
	if status.Complete && (!sourceSweepUsesDurableRetry(kind) || status.PendingRetries == 0) {
		status.LastSuccessfulRound = completedRound
		recordScanRecovery(kind, "source_scan", status, &p.DiagnosticOutbox, "")
		status.LastSuccessAt, status.ErrorRecovered, status.NextAttemptAt = status.ObservedAt, !status.ErrorAt.IsZero(), time.Time{}
	}
	p.Round++
	if kind == "diagnostic_retention_events" {
		p.EventPartitionDate, p.EventRoundEndDate = "", ""
	}
	retry := p.RoundFailed || len(p.Backoff) > 0
	p.RoundFailed, p.Cursor = false, inventory.Cursor{}
	if retry {
		p.IdleRounds = 0
	} else {
		p.IdleRounds++
	}
	p.NextRound = time.Now().Add(s.sourceSweepDelay(kind, p.IdleRounds, retry))
}

func markSourceRetryRoundSuccess(p *sourceSweepCheckpoint) {
	if p.Status.PendingRetries == 0 && p.Status.Complete && p.Round > 0 && p.Status.LastSuccessfulRound < p.Round-1 {
		now := time.Now().UTC()
		p.Status.LastSuccessfulRound = p.Round - 1
		p.Status.LastSuccessAt = now
		p.Status.ErrorRecovered = !p.Status.ErrorAt.IsZero()
		p.Status.NextAttemptAt = time.Time{}
	}
}
