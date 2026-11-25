package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

type diagnosticPartitionBackoff struct {
	mu      sync.Mutex
	retries map[string]diagnosticPartitionRetry
}

type diagnosticPartitionRetry struct {
	dueAt time.Time
	delay time.Duration
}

type diagnosticPartitionDeferred struct{ until time.Time }

func (diagnosticPartitionDeferred) Error() string { return "diagnostic date is in backoff" }

func (b *diagnosticPartitionBackoff) dueAt(date string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retries[date].dueAt
}

func (b *diagnosticPartitionBackoff) deferRetry(date string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retries == nil {
		b.retries = make(map[string]diagnosticPartitionRetry)
	}
	now := time.Now()
	if b.retries[date].dueAt.After(now) {
		return false
	}
	for key, retry := range b.retries {
		if key != date && now.Sub(retry.dueAt) > time.Minute {
			delete(b.retries, key)
		}
	}
	delay := min(max(time.Second, b.retries[date].delay*2), time.Minute)
	jitter := time.Duration(now.UnixNano() % int64(delay/4+1))
	b.retries[date] = diagnosticPartitionRetry{dueAt: now.Add(delay + jitter), delay: delay}
	return true
}

func (b *diagnosticPartitionBackoff) clear(date string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.retries, date)
}

func (s *Service) withDiagnosticPendingPartition(ctx context.Context, date string, force bool, work func(context.Context) error) error {
	if until := s.diagnosticBatchBackoff.dueAt(date); !force && until.After(time.Now()) {
		return diagnosticPartitionDeferred{until: until}
	}
	err := s.withDiagnosticFencedLease(ctx, diagnosticPartitionLeaseKey(date), func(guarded context.Context) error {
		if until := s.diagnosticBatchBackoff.dueAt(date); !force && until.After(time.Now()) {
			return diagnosticPartitionDeferred{until: until}
		}
		err := work(guarded)
		s.deferDiagnosticBatchRetry(date, err)
		return err
	})
	s.deferDiagnosticBatchRetry(date, err)
	return err
}

func (s *Service) deferDiagnosticBatchRetry(date string, err error) {
	if errors.Is(err, storage.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrUnknownCommit) ||
		errors.Is(err, storage.ErrLeaseLost) || errors.Is(err, lease.ErrLost) {
		if s.diagnosticBatchBackoff.deferRetry(date) {
			s.signalDiagnosticBatchWake()
		}
	}
}
