package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

var errSourceBudgetYield = errors.New("source execution budget yielded")

func (s *Service) sourceSweepEntity(ctx context.Context, kind, prefix string, p *sourceSweepCheckpoint, step sourceSweepStep) bool {
	entry, found, err := p.Cursor.Peek(ctx, s.store, prefix, 32)
	if err != nil {
		if ctx.Err() != nil && pureContextBudget(err) {
			return false
		}
		s.failSourceScan(ctx, kind, prefix, p, err)
		return false
	}
	p.ScanRetries = 0
	if !found {
		return !p.Cursor.Done
	}
	id := entry.Key + "\x00" + entry.Version
	if b, exists := p.Backoff[id]; exists {
		b.Round = p.Round
		p.Backoff[id] = b
	}
	for previous := range p.Backoff {
		if strings.HasPrefix(previous, entry.Key+"\x00") && previous != id {
			delete(p.Backoff, previous)
		}
	}
	if !p.Backoff[id].Next.After(time.Now()) {
		err = step(ctx, entry, p)
		if errors.Is(err, errSourceBudgetYield) {
			return false
		}
		if ctx.Err() != nil && pureContextBudget(err) {
			return false
		}
		if s.sourceDependencyWait(ctx, kind, p, err) {
			return false
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
			if sourceSweepUsesDurableRetry(kind) {
				current, exists := p.Backoff[id]
				attempts := 1
				if exists {
					attempts = max(1, current.Retries+1)
				}
				delay := min(time.Second*time.Duration(1<<min(attempts, 8)), 5*time.Minute)
				next := time.Now().UTC().Add(delay + time.Duration(time.Now().UnixNano()%int64(delay/4+1)))
				if enqueueErr := s.enqueueSourceSweepRetry(ctx, kind, entry, err, attempts, next); enqueueErr != nil {
					s.failSourceScan(ctx, kind, prefix, p, enqueueErr)
					return false
				}
				incrementSweepCount(p, "failed", 1)
				p.Status.ErrorAt, p.Status.ErrorEntity, p.Status.ErrorClass, p.Status.NextAttemptAt, p.Status.ErrorRecovered = time.Now().UTC(), entry.Key, diagnosticReason(err), next, false
				s.recordScanFailure(ctx, kind, "source_entity", &p.Status, &p.DiagnosticOutbox, err, uint64(attempts))
				s.backgroundEvent(kind, "entity_failed", err)
			} else {
				p.Status.Complete = false
				b := p.Backoff[id]
				b.Retries++
				b.Round = p.Round
				b.Next = time.Now().Add(min(time.Second*time.Duration(1<<min(b.Retries, 8)), 5*time.Minute))
				b.Next = b.Next.Add(time.Duration(time.Now().UnixNano() % int64(time.Second)))
				if len(p.Backoff) < 1024 || b.Retries > 1 {
					p.Backoff[id] = b
				} else {
					p.NextRound = b.Next
				}
				p.RoundFailed = true
				p.Status.ErrorAt, p.Status.ErrorEntity, p.Status.ErrorClass, p.Status.NextAttemptAt, p.Status.ErrorRecovered = time.Now().UTC(), entry.Key, diagnosticReason(err), b.Next, false
				s.recordScanFailure(ctx, kind, "source_entity", &p.Status, &p.DiagnosticOutbox, err, uint64(b.Retries))
				s.backgroundEvent(kind, "entity_failed", err)
			}
		} else {
			if p.Backoff[id].Retries > 0 {
				recordScanRecovery(kind, "source_entity", &p.Status, &p.DiagnosticOutbox, entry.Key)
			}
			delete(p.Backoff, id)
			p.Status.ProgressAt = time.Now().UTC()
			p.Status.LastEntityProgressAt = p.Status.ProgressAt
			p.Status.WaitReason, p.Status.DependencyGeneration = "", ""
			if p.Status.ErrorEntity == entry.Key {
				p.Status.ErrorRecovered = true
			}
			incrementSweepCount(p, "processed", 1)
		}
	}
	p.Cursor.Advance()
	p.Status.LastCursorProgressAt = time.Now().UTC()
	incrementSweepCount(p, "scanned", 1)
	return !p.Cursor.Done
}

func incrementSweepCount(p *sourceSweepCheckpoint, name string, amount int64) {
	if p.Status.Counts == nil {
		p.Status.Counts = map[string]int64{}
	}
	p.Status.Counts[name] += amount
	if name == "scanned" || name == "processed" || name == "failed" {
		p.Status.Operations++
	}
}

func (s *Service) failSourceScan(ctx context.Context, kind, prefix string, p *sourceSweepCheckpoint, err error) {
	if errors.Is(err, errSourceBudgetYield) || ctx.Err() != nil && pureContextBudget(err) {
		return
	}
	if s.sourceDependencyWait(ctx, kind, p, err) {
		return
	}
	p.ScanRetries++
	p.NextRound = time.Now().Add(min(time.Second*time.Duration(1<<min(p.ScanRetries, 8)), 5*time.Minute))
	p.Status.Complete, p.RoundFailed = false, true
	p.Status.ErrorAt, p.Status.ErrorClass, p.Status.ErrorEntity, p.Status.NextAttemptAt, p.Status.ErrorRecovered = time.Now().UTC(), diagnosticReason(err), prefix, p.NextRound, false
	s.recordScanFailure(ctx, kind, "source_scan", &p.Status, &p.DiagnosticOutbox, err, uint64(p.ScanRetries))
	s.backgroundEvent(kind, "scan_failed", err)
}
