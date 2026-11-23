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
	Recorded   bool
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
	for {
		select {
		case <-ctx.Done():
			return
		case wake := <-s.diagnosticSourceWakes:
			s.diagnosticSourceWakeMu.Lock()
			delete(s.diagnosticSourcePending, wake.Key)
			s.diagnosticSourceWakeMu.Unlock()
			s.updateWorker("diagnostic_source_events", func(state *workerStatus) { state.State = "execution" })
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.collectDiagnosticSourceOutbox(bounded, wake.Key, wake.AdmittedAt)
			cancel()
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
	for attempt := 0; attempt <= diagnostics.OutboxLimit; attempt++ {
		if err := s.collectDiagnosticOutboxVersion(ctx, key, version, events); err != nil {
			return err
		}
		receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, diagnostics.Prefix+"collected/"+diagnostics.ID(key)+".json")
		if err != nil {
			return err
		}
		if diagnosticOutboxSettled(key, version, events, receipt) {
			return nil
		}
	}
	return storage.ErrUnavailable
}
