package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"time"
)

type diagnosticContextKey struct{}
type diagnosticWriteKey struct{}
type diagnosticReadKey struct{}
type backgroundDiagnosticContext struct {
	Kind, RunID, Scope string
	WorkID, AttemptID  string
	Budget, Admission  float64
}

func (s *Service) emitDiagnosticEvent(event diagnostics.Event) {
	if s.diagnosticEvents == nil {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.ID == "" {
		event.ID = diagnostics.ID(event.Component, event.Operation, event.OccurredAt.Format(time.RFC3339Nano), requestID())
	}
	if event.Responsibility == "" {
		event.Responsibility = "unknown"
	}
	if event.TimeoutScope == "" {
		event.TimeoutScope = "unknown"
	}
	if event.AttemptLayer == "" {
		event.AttemptLayer = "process_observation"
	}
	select {
	case s.diagnosticEvents <- event:
	default:
		s.diagnosticEventsDropped.Add(1)
	}
}

func (s *Service) recordDiagnosticStorageFailure(ctx context.Context, operation string, cause error, started time.Time) {
	if ctx.Value(diagnosticWriteKey{}) != nil || ctx.Value(diagnosticReadKey{}) != nil {
		return
	}
	duration := time.Since(started).Seconds()
	event := diagnostics.Event{Component: "storage", Operation: operation, Stage: "storage_call", Result: "failed", OccurredAt: started.UTC(), ErrorClass: diagnosticReason(cause), Reason: "storage_operation_failed", ExecutionDurationSeconds: &duration, TimeoutScope: "storage_call", AttemptLayer: "storage_call"}
	if diagnostic, ok := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		event.WorkKind, event.RunID = diagnostic.Kind, diagnostic.RunID
		event.WorkID, event.AttemptID = diagnostic.WorkID, diagnostic.AttemptID
		event.AdmissionWaitSeconds = &diagnostic.Admission
		event.ExecutionBudgetSeconds = &diagnostic.Budget
		if ctx.Err() != nil {
			event.TimeoutScope = diagnostic.Scope
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := max(0, deadline.Sub(started).Seconds())
		event.DeadlineRemainingSeconds = &remaining
	}
	if errors.Is(cause, context.Canceled) {
		event.Reason = "context_cancelled"
	}
	if meta, ok := ctx.Value(requestMetaKey{}).(*requestMeta); ok {
		event.RequestID = meta.requestID
	}
	if id, ok := ctx.Value(storageOperationIDKey{}).(string); ok {
		event.OperationID = id
	}
	var awsRequest interface{ S3RequestID() string }
	if errors.As(cause, &awsRequest) {
		event.S3RequestID = awsRequest.S3RequestID()
	}
	s.emitDiagnosticEvent(event)
}

func (s *Service) runDiagnosticEventSink(ctx context.Context) {
	diagnosticConfig := s.diagnosticsConfig()
	ordinaryFlushInterval := diagnosticConfig.OrdinaryEventFlushInterval
	criticalEvents, ordinaryEvents := []diagnostics.Event{}, []diagnostics.Event{}
	var criticalBytes, ordinaryBytes int
	criticalDue, ordinaryDue := time.Time{}, time.Time{}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-s.diagnosticEvents:
			encoded, err := json.Marshal(event)
			if err != nil || len(encoded)+256 > diagnostics.BatchMaxBytes {
				s.diagnosticEventsDropped.Add(1)
				continue
			}
			if diagnosticCriticalEvent(event) {
				criticalEvents, criticalBytes = append(criticalEvents, event), criticalBytes+len(encoded)+1
				if criticalDue.IsZero() {
					criticalDue = time.Now().Add(diagnosticConfig.CriticalEventFlushInterval)
				}
				if len(criticalEvents) >= diagnostics.BatchMaxEvents || criticalBytes+256 > diagnostics.BatchMaxBytes {
					s.flushDiagnosticEvents(ctx, criticalEvents, true)
					criticalEvents, criticalBytes, criticalDue = nil, 0, time.Time{}
				}
			} else {
				ordinaryEvents, ordinaryBytes = append(ordinaryEvents, event), ordinaryBytes+len(encoded)+1
				if ordinaryDue.IsZero() {
					ordinaryDue = time.Now().Add(ordinaryFlushInterval)
				}
				if len(ordinaryEvents) >= diagnostics.BatchMaxEvents || ordinaryBytes+256 > diagnostics.BatchMaxBytes {
					s.flushDiagnosticEvents(ctx, ordinaryEvents, false)
					ordinaryEvents, ordinaryBytes, ordinaryDue = nil, 0, time.Time{}
				}
			}
		case now := <-ticker.C:
			if !criticalDue.IsZero() && !now.Before(criticalDue) {
				s.flushDiagnosticEvents(ctx, criticalEvents, true)
				criticalEvents, criticalBytes, criticalDue = nil, 0, time.Time{}
			}
			if !ordinaryDue.IsZero() && !now.Before(ordinaryDue) {
				s.flushDiagnosticEvents(ctx, ordinaryEvents, false)
				ordinaryEvents, ordinaryBytes, ordinaryDue = nil, 0, time.Time{}
			}
		}
	}
}

func (s *Service) flushDiagnosticEvents(ctx context.Context, events []diagnostics.Event, critical bool) {
	if len(events) == 0 {
		return
	}
	s.updateWorker("diagnostic_events", func(state *workerStatus) { state.State = "execution" })
	bounded, cancel := context.WithTimeout(context.WithValue(ctx, diagnosticWriteKey{}, true), 5*time.Second)
	defer cancel()
	for offset := 0; offset < len(events); {
		end := diagnosticBatchEnd(events, offset)
		if end == offset {
			s.diagnosticEventsDropped.Add(uint64(len(events) - offset))
			return
		}
		_, err := (diagnostics.BatchLog{Store: s.store, Retention: s.diagnosticsConfig().EventRetention}).Append(bounded, events[offset:end], critical)
		if err != nil {
			s.diagnosticEventsDropped.Add(uint64(end - offset))
			if errors.Is(err, diagnostics.ErrHistoryCapacity) {
				_ = s.recordDiagnosticBatchGap(bounded, end-offset, "batch_budget_capacity")
			} else if errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
				_ = s.recordDiagnosticBatchGap(bounded, end-offset, "event_history_expired")
			} else {
				_ = s.recordDiagnosticBatchGap(bounded, end-offset, "batch_write_failed")
			}
		}
		offset = end
	}
	s.updateWorker("diagnostic_events", func(state *workerStatus) { state.State = "idle" })
}
