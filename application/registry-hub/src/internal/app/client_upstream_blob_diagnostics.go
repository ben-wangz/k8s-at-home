package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

type upstreamBlobStageFailure struct {
	stage string
	err   error
}

func shouldLogUpstreamBlobFetchFailure(err *upstreamBlobFetchError) bool {
	return err != nil && (err.status == 0 || err.status >= 500)
}

func (e *upstreamBlobStageFailure) Error() string {
	return fmt.Sprintf("upstream blob failed at %s: %v", e.stage, e.err)
}

func (e *upstreamBlobStageFailure) Unwrap() error { return e.err }

func withUpstreamBlobStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *upstreamBlobStageFailure
	if errors.As(err, &existing) {
		return err
	}
	var persistenceFailure *download.PersistenceError
	if errors.As(err, &persistenceFailure) {
		return err
	}
	var guardFailure *download.GuardError
	if errors.As(err, &guardFailure) {
		return err
	}
	return &upstreamBlobStageFailure{stage: stage, err: err}
}

func upstreamBlobFailureStage(err error, fallback string) string {
	if stage := artifact.CacheReadFailureStage(err); stage != "" {
		return "cache_" + stage
	}
	var stageFailure *upstreamBlobStageFailure
	if errors.As(err, &stageFailure) {
		return stageFailure.stage
	}
	var persistenceFailure *download.PersistenceError
	if errors.As(err, &persistenceFailure) && persistenceFailure.Stage != "" {
		return persistenceFailure.Stage
	}
	var guardFailure *download.GuardError
	if errors.As(err, &guardFailure) {
		return "download_guard"
	}
	if errors.Is(err, download.ErrQuotaExceeded) {
		return "download_quota"
	}
	var fetchFailure *upstreamBlobFetchError
	if errors.As(err, &fetchFailure) {
		return "upstream_fetch"
	}
	return fallback
}

func upstreamBlobFailureCause(err error) string {
	switch {
	case errors.Is(err, lease.ErrLost), errors.Is(err, storage.ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, lease.ErrHeld):
		return "lease_held"
	case errors.Is(err, download.ErrQuotaExceeded):
		return "quota"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return storageResult(err)
	}
}

func recordUpstreamBlobFailure(meta *requestMeta, fallback string, err error, elapsed time.Duration) {
	if meta == nil || err == nil {
		return
	}
	meta.failureStage = upstreamBlobFailureStage(err, fallback)
	meta.failureCause = upstreamBlobFailureCause(err)
	meta.failureElapsedMS = elapsed.Milliseconds()
}
