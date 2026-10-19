package app

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"time"
)

func (s *solidifier) downloadBlobObject(ctx context.Context, task *tasks.Task, taskLease lease.Lease, ref contentRef, objectLease lease.Lease) error {
	maxBytes := s.cfg.Limits.ObjectBytes
	if ref.size <= 0 || ref.size > maxBytes {
		return permanent("object_limit", errors.New("blob size is invalid or exceeds limit"))
	}
	requestTimeout := s.cfg.Limits.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 300 * time.Second
	}
	transferTimeout := s.cfg.Limits.BlobTransferTimeout
	if transferTimeout <= 0 {
		transferTimeout = 24 * time.Hour
	}
	idleTimeout := s.cfg.Limits.UpstreamBodyIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	transferCtx, cancelTransfer := context.WithTimeout(ctx, transferTimeout)
	defer cancelTransfer()
	identity := objectLeaseKey(task.Authority, task.Repository, ref.digest)
	manager := s.service.downloadManager()
	requestPath := "/v2/" + task.Repository + "/blobs/" + url.PathEscape(ref.digest)
	result, err := manager.Fetch(transferCtx, download.Request{
		Identity: identity, Digest: ref.digest, ExpectedSize: ref.size, MaxBytes: maxBytes,
		OwnerFence: objectLease.FencingToken, AttemptLimit: requestTimeout,
		IdleTimeout: idleTimeout,
		Retryable:   upstream.Retryable,
		WaitRetry:   s.service.downloadRetryWaiter(),
		Observe:     s.service.metrics.recordEvent,
		Guard: func(guardCtx context.Context) error {
			if err := s.tasks.Leases().Validate(guardCtx, objectLease); err != nil {
				return err
			}
			return s.tasks.CheckActive(guardCtx, task, taskLease)
		},
		Source: func(sourceCtx context.Context, start, end int64, etag string) (*http.Response, context.CancelFunc, error) {
			attemptCtx, cancel := context.WithCancel(sourceCtx)
			headers := make(http.Header)
			if start > 0 {
				headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
				if etag != "" {
					headers.Set("If-Range", etag)
				}
			}
			response, fetchErr := s.service.fetchUpstreamOnce(attemptCtx, http.MethodGet, task.Authority, task.Repository, requestPath, nil, headers)
			if fetchErr != nil {
				cancel()
				return nil, nil, fetchErr
			}
			return response, cancel, nil
		},
	})
	if err != nil {
		var statusErr *download.StatusError
		if errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound {
			return permanent("upstream_not_found", err)
		}
		return temporary("upstream", err)
	}
	mediaType := blobCacheMediaType(ref.mediaType, result.MediaType)
	body, size, err := manager.Open(transferCtx, identity, ref.digest, maxBytes)
	if err != nil {
		return temporary("storage", err)
	}
	defer body.Close()
	if err := s.cache.PutChecked(transferCtx, task.Authority, task.Repository, ref.digest, mediaType, body, size, func() error {
		if err := s.tasks.Leases().Validate(transferCtx, objectLease); err != nil {
			return err
		}
		return s.tasks.CheckActive(transferCtx, task, taskLease)
	}); err != nil {
		if errors.Is(err, artifact.ErrDigestMismatch) {
			_ = manager.Discard(transferCtx, identity, ref.digest, objectLease.FencingToken)
			return permanent("digest_mismatch", artifact.ErrDigestMismatch)
		}
		if errors.Is(err, storage.ErrLeaseLost) {
			return err
		}
		return temporary("storage", err)
	}
	if err := s.tasks.Leases().Validate(transferCtx, objectLease); err != nil {
		return err
	}
	if err := manager.CommitComplete(transferCtx, identity, ref.digest, objectLease.FencingToken); err != nil {
		return temporary("storage", err)
	}
	ref.mediaType = mediaType
	return s.checkpoint(transferCtx, task, taskLease, ref)
}

func blobCacheMediaType(descriptorMediaType, responseMediaType string) string {
	if descriptorMediaType != "" {
		return descriptorMediaType
	}
	if parsed, _, err := mime.ParseMediaType(responseMediaType); err == nil && parsed != "" {
		return parsed
	}
	return "application/octet-stream"
}
