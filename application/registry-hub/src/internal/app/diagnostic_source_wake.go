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

type diagnosticSourceWake struct {
	Key        string
	AdmittedAt time.Time
}

type diagnosticSourceWakeTimingKey struct{}

type diagnosticSourceWakeTiming struct {
	AdmittedAt time.Time
}

const diagnosticPendingRecoveryInterval = time.Minute

func (s *Service) signalDiagnosticBatchWake() {
	if s.diagnosticBatchWake == nil {
		return
	}
	select {
	case s.diagnosticBatchWake <- struct{}{}:
	default:
	}
}

func (s *Service) queueDiagnosticSourceWake(key string, committedAt time.Time) {
	if s.diagnosticSourceWakes == nil || !diagnosticOutboxSourceKey(key) {
		return
	}
	s.diagnosticSourceWakeMu.Lock()
	if s.diagnosticSourcePending == nil {
		s.diagnosticSourcePending = make(map[string]struct{})
	}
	if _, exists := s.diagnosticSourcePending[key]; exists {
		s.diagnosticSourceWakeMu.Unlock()
		return
	}
	s.diagnosticSourcePending[key] = struct{}{}
	select {
	case s.diagnosticSourceWakes <- diagnosticSourceWake{Key: key, AdmittedAt: time.Now().UTC()}:
		s.diagnosticSourceWakeMu.Unlock()
		if !committedAt.IsZero() {
			s.metrics.recordDiagnosticWakeLatency("source_commit_to_admission", time.Since(committedAt))
		}
	default:
		delete(s.diagnosticSourcePending, key)
		s.diagnosticSourceWakeMu.Unlock()
		s.diagnosticSourceDrops.Add(1)
		if s.metrics != nil {
			s.metrics.recordEvent("diagnostic_source_wake_dropped")
		}
	}
}

func diagnosticOutboxSourceKey(key string) bool {
	return strings.HasPrefix(key, "tasks/state/") || strings.HasPrefix(key, "solidification/blobs/intents/") ||
		strings.HasPrefix(key, workqueue.Prefix+"jobs/") || strings.HasPrefix(key, workqueue.Prefix+"receipts/") ||
		strings.HasPrefix(key, "downloads/v1/") && strings.HasSuffix(key, "/state.json") || strings.HasPrefix(key, "uploads/sessions/")
}

func (s *Service) runDiagnosticSourceWakeSink(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var nextDeadline, retryAt time.Time
	retryDelay := time.Second
	resetTimer := func(delay time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
	}
	scheduleScan := func(now time.Time) {
		next := now.Add(diagnosticPendingRecoveryInterval)
		if !nextDeadline.IsZero() && nextDeadline.Before(next) {
			next = nextDeadline
		}
		resetTimer(time.Until(next))
	}
	scan := func(now time.Time) {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		deadline, batchErr, scanErr := s.scanDiagnosticPendingBatches(bounded, now.UTC())
		cancel()
		if err := errors.Join(batchErr, scanErr); err != nil && ctx.Err() == nil {
			s.diagnosticSourceErrors.Add(1)
			if s.metrics != nil {
				s.metrics.recordEvent("diagnostic_source_batch_flush_failed")
			}
			s.updateWorker("diagnostic_source_events", func(state *workerStatus) {
				state.State, state.ErrorClass = "backoff", diagnosticReason(err)
			})
		}
		if scanErr != nil {
			retryAt = time.Now().Add(retryDelay)
			if !deadline.IsZero() && deadline.Before(retryAt) {
				retryAt = deadline
			}
			retryDelay = min(retryDelay*2, time.Minute)
			resetTimer(time.Until(retryAt))
			return
		}
		nextDeadline = deadline
		retryAt = time.Time{}
		retryDelay = time.Second
		if batchErr == nil {
			s.updateWorker("diagnostic_source_events", func(state *workerStatus) {
				state.State, state.ErrorClass = "idle", ""
			})
		}
		scheduleScan(now)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			scan(now.UTC())
		case <-s.diagnosticBatchWake:
			now := time.Now().UTC()
			if retryAt.IsZero() || !now.Before(retryAt) {
				scan(now)
			}
		case wake := <-s.diagnosticSourceWakes:
			s.diagnosticSourceWakeMu.Lock()
			delete(s.diagnosticSourcePending, wake.Key)
			s.diagnosticSourceWakeMu.Unlock()
			s.updateWorker("diagnostic_source_events", func(state *workerStatus) { state.State = "execution" })
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.collectDiagnosticSourceOutbox(bounded, wake.Key, wake.AdmittedAt)
			cancel()
			var deferred diagnosticPartitionDeferred
			if errors.As(err, &deferred) {
				s.updateWorker("diagnostic_source_events", func(state *workerStatus) { state.State = "backoff" })
				continue
			}
			if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) && ctx.Err() == nil {
				s.diagnosticSourceErrors.Add(1)
				if s.metrics != nil {
					s.metrics.recordEvent("diagnostic_source_wake_failed")
				}
				s.updateWorker("diagnostic_source_events", func(state *workerStatus) {
					state.State, state.ErrorClass = "backoff", diagnosticReason(err)
				})
				continue
			}
			s.updateWorker("diagnostic_source_events", func(state *workerStatus) {
				state.State, state.ErrorClass = "idle", ""
			})
		}
	}
}

func (s *Service) collectDiagnosticSourceOutbox(ctx context.Context, key string, admittedAt time.Time) error {
	if !diagnosticOutboxSourceKey(key) {
		return nil
	}
	raw, version, err := inventory.Read[json.RawMessage](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	entry, err := s.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if version != "" && version != sourceEntryVersion(entry) {
		return storage.ErrConditionFailed
	}
	if version == "" {
		version = sourceEntryVersion(entry)
	}
	var source struct {
		DiagnosticOutbox []diagnostics.Event `json:"diagnosticOutbox"`
	}
	if err := json.Unmarshal(raw, &source); err != nil || len(source.DiagnosticOutbox) == 0 {
		return err
	}
	events := source.DiagnosticOutbox
	timing := &diagnosticSourceWakeTiming{AdmittedAt: admittedAt}
	ctx = context.WithValue(ctx, diagnosticSourceWakeTimingKey{}, timing)
	if strings.HasPrefix(key, workqueue.Prefix+"jobs/") || strings.HasPrefix(key, workqueue.Prefix+"receipts/") {
		var job workqueue.Job
		if err := json.Unmarshal(raw, &job); err != nil {
			return err
		}
		association := diagnosticJob(job)
		for index := range events {
			events[index].Authority, events[index].Repository, events[index].Digest = association.Authority, association.Repository, association.Digest
			events[index].TaskID, events[index].IntentID, events[index].TransferID, events[index].ImageID = association.TaskID, association.IntentID, association.TransferID, association.ImageID
		}
	}
	return s.collectDiagnosticOutboxVersion(ctx, key, version, events)
}
