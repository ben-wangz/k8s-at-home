package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) logSolidificationResult(task *tasks.Task, p *solidificationPerformance, result error) {
	if s.service == nil || s.service.logger == nil {
		return
	}
	if result == nil {
		if task.Kind == tasks.RootKind {
			s.logSolidificationRoot(task, p)
		}
		return
	}
	var yielded *tasks.Yield
	if errors.Is(result, context.Canceled) || errors.As(result, &yielded) {
		return
	}
	class := "solidification"
	var failure *tasks.Failure
	if errors.As(result, &failure) {
		class = failure.Class
	}
	if errors.Is(result, lease.ErrLost) {
		class = "fencing"
	} else if errors.Is(result, artifact.ErrDigestMismatch) {
		class = "digest_mismatch"
	}
	if errors.Is(result, artifact.ErrDigestMismatch) {
		s.service.metrics.recordEvent("digest_mismatch")
	}
	if errors.Is(result, lease.ErrLost) || errors.Is(result, storage.ErrLeaseLost) {
		s.service.metrics.recordEvent("lease_lost")
	}
	fields := append(solidificationTaskIdentityLogFields(task), p.fields()...)
	fields = append(fields, "error_class", class)
	var stage *solidificationFailureStage
	if errors.As(result, &failure) && errors.As(failure.Err, &stage) {
		fields = append(fields, "failure_stage", stage.name)
		if stage.sessionID != "" {
			fields = append(fields, "session_id", stage.sessionID)
		}
	}
	var downloadErr *download.PersistenceError
	if errors.As(result, &downloadErr) {
		fields = append(fields, "download_stage", downloadErr.Stage)
	}
	fields = append(fields, "error", result)
	s.service.logger.Warn("solidification failed", fields...)
}
