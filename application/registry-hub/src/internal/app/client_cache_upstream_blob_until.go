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
	"registry-hub/internal/lease"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
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
		_ = s.tasks.Leases().Release(context.Background(), objectLease)
	}()
	if cached, err := s.cache.GetAny(leaseCtx, routed.Upstream, routed.RemoteRepo, routed.Reference); err == nil {
		_ = cached.Body.Close()
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	headers := requestHeaders(nil)
	response, err := s.fetchUpstreamMethod(leaseCtx, http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, headers)
	if err != nil {
		return &upstreamBlobFetchError{err: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &upstreamBlobFetchError{status: response.StatusCode, err: fmt.Errorf("upstream blob returned status %d", response.StatusCode)}
	}
	hasher, err := artifact.Hasher(routed.Reference)
	if err != nil {
		return &upstreamBlobFetchError{err: err}
	}
	spool, size, err := spoolResponse(response.Body, s.cfg.Limits.ObjectBytes, hasher)
	if err != nil {
		return &upstreamBlobFetchError{err: err}
	}
	defer removeSpool(spool)
	if response.ContentLength >= 0 && response.ContentLength != size {
		return &upstreamBlobFetchError{err: errors.New("upstream blob length does not match Content-Length")}
	}
	if artifact.DigestFromHasher(routed.Reference, hasher) != routed.Reference {
		return &upstreamBlobFetchError{err: artifact.ErrDigestMismatch}
	}
	if digestHeader := response.Header.Get("Docker-Content-Digest"); digestHeader != "" && digestHeader != routed.Reference {
		return &upstreamBlobFetchError{err: fmt.Errorf("%w: upstream blob digest header does not match the request", artifact.ErrDigestMismatch)}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if err := s.tasks.Leases().Validate(leaseCtx, objectLease); err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return &upstreamBlobFetchError{err: err}
	}
	if err := s.cache.PutChecked(leaseCtx, routed.Upstream, routed.RemoteRepo, routed.Reference, mediaType, spool, size, func() error {
		return s.tasks.Leases().Validate(leaseCtx, objectLease)
	}); err != nil {
		return err
	}
	if err := s.tasks.Leases().Validate(leaseCtx, objectLease); err != nil {
		return err
	}
	return nil
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
