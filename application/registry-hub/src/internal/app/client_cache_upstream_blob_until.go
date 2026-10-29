package app

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/upstream"
	"time"
)

func (s *Service) cacheUpstreamBlobUntil(ctx context.Context, routed route.Route, deadline time.Time) error {
	identity := objectLeaseKey(routed.Upstream, routed.RemoteRepo, routed.Reference)
	owner := requestID()
	for {
		if cached, err := s.cache.GetAny(ctx, routed.Upstream, routed.RemoteRepo, routed.Reference); err == nil {
			_ = cached.Body.Close()
			return nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		objectLease, err := s.tasks.Leases().Acquire(ctx, "objects/"+identity, owner, objectLeaseTTL)
		if err == nil {
			return s.downloadSharedBlob(ctx, routed, objectLease)
		}
		if !errors.Is(err, lease.ErrHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return &upstreamBlobFetchError{
				status: http.StatusServiceUnavailable, retryAfter: "5",
				err: errors.New("timed out waiting for shared object download"),
			}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) downloadSharedBlob(ctx context.Context, routed route.Route, objectLease lease.Lease) (result error) {
	leaseCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go renewLease(leaseCtx, cancel, s.tasks.Leases(), objectLease, renewDone)
	defer func() {
		cancel()
		if renewErr := <-renewDone; result == nil && renewErr != nil {
			result = renewErr
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(leaseCtx), 5*time.Second)
		defer releaseCancel()
		_ = s.tasks.Leases().Release(releaseCtx, objectLease)
	}()
	if cached, err := s.cache.GetAny(leaseCtx, routed.Upstream, routed.RemoteRepo, routed.Reference); err == nil {
		_ = cached.Body.Close()
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	identity := objectLeaseKey(routed.Upstream, routed.RemoteRepo, routed.Reference)
	maxBytes := s.cfg.Limits.ObjectBytes
	if maxBytes <= 0 {
		maxBytes = 100 << 30
	}
	requestTimeout := s.cfg.Limits.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 300 * time.Second
	}
	idleTimeout := s.cfg.Limits.UpstreamBodyIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	transferTimeout := s.cfg.Limits.BlobTransferTimeout
	if transferTimeout <= 0 {
		transferTimeout = 24 * time.Hour
	}
	finalizeTimeout := s.cfg.Limits.DownloadFinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = 300 * time.Second
	}
	transferCtx, transferCancel := context.WithTimeout(leaseCtx, transferTimeout)
	defer transferCancel()
	manager := s.downloadManager()
	downloadResult, err := manager.Fetch(transferCtx, download.Request{
		Authority: routed.Upstream, Repository: routed.RemoteRepo,
		Identity: identity, Digest: routed.Reference, ExpectedSize: -1, MaxBytes: maxBytes,
		OwnerFence: objectLease.FencingToken, AttemptLimit: requestTimeout,
		IdleTimeout: idleTimeout, FinalizeTimeout: finalizeTimeout, LifecycleContext: s.ctx,
		Guard:     func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, objectLease) },
		Retryable: upstream.Retryable,
		WaitRetry: s.downloadRetryWaiter(),
		Observe:   s.metrics.recordEvent,
		Source: func(ctx context.Context, start, end int64, etag string) (*http.Response, context.CancelFunc, error) {
			attemptCtx, attemptCancel := context.WithCancel(ctx)
			headers := requestHeaders(nil)
			if start > 0 {
				headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
				if etag != "" {
					headers.Set("If-Range", etag)
				}
			}
			response, fetchErr := s.fetchUpstreamOnce(attemptCtx, http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, headers)
			if fetchErr != nil {
				attemptCancel()
				return nil, nil, fetchErr
			}
			return response, attemptCancel, nil
		},
	})
	if err != nil {
		var persistenceErr *download.PersistenceError
		var guardErr *download.GuardError
		if errors.As(err, &persistenceErr) || errors.As(err, &guardErr) || errors.Is(err, download.ErrQuotaExceeded) {
			return err
		}
		var statusErr *download.StatusError
		if errors.As(err, &statusErr) {
			return &upstreamBlobFetchError{status: statusErr.Code, retryAfter: statusErr.RetryAfter, err: err}
		}
		return &upstreamBlobFetchError{err: err}
	}
	mediaType, _, parseErr := mime.ParseMediaType(downloadResult.MediaType)
	if parseErr != nil || mediaType == "" {
		mediaType = "application/octet-stream"
	}
	body, size, err := manager.Open(transferCtx, identity, routed.Reference, maxBytes)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := s.cache.PutChecked(transferCtx, routed.Upstream, routed.RemoteRepo, routed.Reference, mediaType, body, size, func() error {
		return s.tasks.Leases().Validate(transferCtx, objectLease)
	}); err != nil {
		if errors.Is(err, artifact.ErrDigestMismatch) {
			_ = manager.Discard(transferCtx, identity, routed.Reference, objectLease.FencingToken)
		}
		return err
	}
	if err := s.tasks.Leases().Validate(transferCtx, objectLease); err != nil {
		return err
	}
	s.deferDownloadCleanup(transferCtx, routed.Upstream, routed.RemoteRepo, identity, routed.Reference, objectLease.FencingToken)
	return nil
}
