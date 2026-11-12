package app

import (
	"context"
	"registry-hub/internal/tasks"
	"time"
)

type solidificationPerformanceKey struct{}

// One processor attempt owns these observations; they add no storage work.
type solidificationPerformance struct {
	started         time.Time
	cacheOpen       time.Duration
	upload          time.Duration
	verify          time.Duration
	readback        time.Duration
	uploadedBytes   int64
	readbackBytes   int64
	source          string
	discovery       time.Duration
	discoveryReused bool
	manifestCount   int
	blobCount       int
	closureBytes    int64
}

func withSolidificationPerformance(ctx context.Context) (context.Context, *solidificationPerformance) {
	if observation, ok := ctx.Value(solidificationPerformanceKey{}).(*solidificationPerformance); ok {
		return ctx, observation
	}
	observation := &solidificationPerformance{started: time.Now()}
	return context.WithValue(ctx, solidificationPerformanceKey{}, observation), observation
}

func solidificationTaskLogFields(task *tasks.Task) []any {
	fields := solidificationTaskIdentityLogFields(task)
	if task.RootDigest != "" {
		fields = append(fields, "root_digest", task.RootDigest)
	}
	if task.BlobDigest != "" {
		fields = append(fields, "blob_digest", task.BlobDigest)
	}
	return fields
}

func solidificationTaskIdentityLogFields(task *tasks.Task) []any {
	fields := []any{
		"task_id", task.ID, "task_kind", task.Kind, "round_id", task.RoundID, "attempt_id", task.AttemptID,
		"authority", task.Authority, "repository", task.Repository, "generation", task.Generation,
		"retry_count", task.RetryCount,
	}
	if !task.CreatedAt.IsZero() {
		fields = append(fields, "task_age_seconds", time.Since(task.CreatedAt).Seconds())
	}
	return fields
}

func (p *solidificationPerformance) fields() []any {
	return []any{
		"execution_seconds", time.Since(p.started).Seconds(),
		"cache_open_seconds", p.cacheOpen.Seconds(), "upload_seconds", p.upload.Seconds(),
		"distribution_verify_seconds", p.verify.Seconds(),
		"distribution_readback_seconds", p.readback.Seconds(),
		"uploaded_bytes", p.uploadedBytes, "distribution_readback_bytes", p.readbackBytes,
	}
}

func (s *solidifier) logSolidificationBlob(task *tasks.Task, blob contentRef, p *solidificationPerformance, result *error) {
	if *result != nil || s.service == nil || s.service.logger == nil {
		return
	}
	fields := append(solidificationTaskLogFields(task), p.fields()...)
	fields = append(fields, "digest", blob.digest, "size_bytes", blob.size, "source", p.source)
	s.service.logger.Info("solidification blob verified", fields...)
}

func (s *solidifier) logSolidificationRoot(task *tasks.Task, p *solidificationPerformance) {
	fields := append(solidificationTaskLogFields(task), p.fields()...)
	fields = append(fields, "discovery_seconds", p.discovery.Seconds(), "discovery_reused", p.discoveryReused,
		"manifest_count", p.manifestCount, "blob_count", p.blobCount, "closure_bytes", p.closureBytes)
	s.service.logger.Info("solidification root completed", fields...)
}

func (s *solidifier) logSolidificationAnchor(task *tasks.Task, manifest *stagedManifest, session protectionSessionRecord) {
	if s.service == nil || s.service.logger == nil {
		return
	}
	fields := solidificationTaskLogFields(task)
	fields = append(fields, "session_id", manifest.sessionID, "digest", manifest.ref.digest,
		"media_type", manifest.ref.mediaType, "scope_bytes", manifest.scopeBytes, "blob_count", len(manifest.blobs))
	if !session.CreatedAt.IsZero() {
		fields = append(fields, "anchor_age_seconds", time.Since(session.CreatedAt).Seconds())
	}
	s.service.logger.Info("solidification manifest anchored", fields...)
}

func (s *solidifier) verifySolidificationBlob(ctx context.Context, task *tasks.Task, blob contentRef, path string, p *solidificationPerformance) error {
	started := time.Now()
	defer func() { p.verify += time.Since(started) }()
	return s.verifyOrReuseLocalBlob(ctx, task, blob, taskProtectionSession(task), path)
}
