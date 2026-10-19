package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/upstream"
	"strconv"
	"strings"
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
	transferCtx, transferCancel := context.WithTimeout(leaseCtx, transferTimeout)
	defer transferCancel()
	manager := s.downloadManager()
	downloadResult, err := manager.Fetch(transferCtx, download.Request{
		Identity: identity, Digest: routed.Reference, ExpectedSize: -1, MaxBytes: maxBytes,
		OwnerFence: objectLease.FencingToken, AttemptLimit: requestTimeout,
		IdleTimeout: idleTimeout,
		Guard:       func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, objectLease) },
		Retryable:   upstream.Retryable,
		WaitRetry:   s.downloadRetryWaiter(),
		Observe:     s.metrics.recordEvent,
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
	return manager.CommitComplete(transferCtx, identity, routed.Reference, objectLease.FencingToken)
}

func (s *Service) serveUpstreamBlobRange(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response) {
	spool, size, err := spoolResponse(response.Body, s.cfg.Limits.ObjectBytes, nil)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_TOO_LARGE", err.Error())
		return
	}
	defer removeSpool(spool)
	start, end, total, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", err.Error())
		return
	}
	if total > s.cfg.Limits.ObjectBytes {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_TOO_LARGE", "upstream blob exceeds the configured object limit")
		return
	}
	expectedStart, expectedEnd, _, err := requestedRange(request.Header.Get("Range"), total)
	if err != nil || start != expectedStart || end != expectedEnd || size != end-start+1 {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream Content-Range does not match the requested range")
		return
	}
	if response.ContentLength >= 0 && response.ContentLength != size {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream partial blob length does not match Content-Length")
		return
	}
	if upstreamDigest := response.Header.Get("Docker-Content-Digest"); upstreamDigest != "" && upstreamDigest != routed.Reference {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream blob digest header does not match the request")
		return
	}

	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	writer.Header().Set("Docker-Content-Digest", routed.Reference)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(http.StatusPartialContent)
	if _, err := spool.Seek(0, io.SeekStart); err == nil {
		_, _ = io.Copy(writer, spool)
	}
}

func spoolResponse(body io.Reader, limit int64, digestWriter io.Writer) (*os.File, int64, error) {
	spool, err := os.CreateTemp("", "registry-hub-upstream-")
	if err != nil {
		return nil, 0, err
	}
	remove := func(cause error) (*os.File, int64, error) {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
		return nil, 0, cause
	}
	writer := io.Writer(spool)
	if digestWriter != nil {
		writer = io.MultiWriter(spool, digestWriter)
	}
	size, err := io.Copy(writer, io.LimitReader(body, limit+1))
	if err != nil {
		return remove(err)
	}
	if size > limit {
		return remove(fmt.Errorf("response exceeds the configured object limit of %d bytes", limit))
	}
	return spool, size, nil
}

func removeSpool(spool *os.File) {
	if spool == nil {
		return
	}
	name := spool.Name()
	_ = spool.Close()
	_ = os.Remove(name)
}

func parseContentRange(value string) (int64, int64, int64, error) {
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || parts[0] != "bytes" {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	values := strings.SplitN(parts[1], "/", 2)
	if len(values) != 2 || values[1] == "*" {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	span := strings.SplitN(values[0], "-", 2)
	if len(span) != 2 {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	start, err := strconv.ParseInt(span[0], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("invalid Content-Range start")
	}
	end, err := strconv.ParseInt(span[1], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("invalid Content-Range end")
	}
	total, err := strconv.ParseInt(values[1], 10, 64)
	if err != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, errors.New("invalid Content-Range values")
	}
	return start, end, total, nil
}
