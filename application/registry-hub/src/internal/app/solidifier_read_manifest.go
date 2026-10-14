package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func (s *solidifier) readManifest(ctx context.Context, authority, repository string, reference contentRef) ([]byte, contentRef, error) {
	object, err := s.cache.GetRetained(ctx, authority, repository, reference.digest, reference.mediaType)
	if errors.Is(err, storage.ErrNotFound) && reference.mediaType == "" {
		object, err = s.cache.GetAnyRetained(ctx, authority, repository, reference.digest)
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, contentRef{}, storage.ErrNotFound
		}
		return nil, contentRef{}, temporary("storage", err)
	}
	defer object.Body.Close()
	if object.Size > s.cfg.Limits.ManifestBytes {
		return nil, contentRef{}, permanent("manifest_limit", errors.New("manifest exceeds size limit"))
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return nil, contentRef{}, temporary("storage", err)
	}
	if int64(len(data)) > s.cfg.Limits.ManifestBytes {
		return nil, contentRef{}, permanent("manifest_limit", errors.New("manifest exceeds size limit"))
	}
	if int64(len(data)) != object.Size {
		return nil, contentRef{}, permanent("manifest_invalid", errors.New("cached manifest size verification failed"))
	}
	if err := artifact.VerifyDigest(reference.digest, data); err != nil {
		return nil, contentRef{}, permanent("digest_mismatch", err)
	}
	return data, contentRef{digest: object.Digest, mediaType: object.MediaType, size: object.Size}, nil
}

func (s *solidifier) fetchObject(ctx context.Context, task *tasks.Task, taskLease lease.Lease, ref contentRef, manifest bool) error {
	if cached, err := s.cacheHas(ctx, task, ref); err != nil {
		return temporary("storage", err)
	} else if cached {
		return nil
	}
	identity := objectLeaseKey(task.Authority, task.Repository, ref.digest)
	owner := task.ID + "-" + strings.ReplaceAll(ref.digest, ":", "-")
	deadline := time.Now().Add(30 * time.Second)
	waiting := false
	defer func() {
		if waiting {
			s.service.metrics.objectLeases.WithLabelValues("waiting").Dec()
		}
	}()
	for {
		cached, cacheErr := s.cacheHas(ctx, task, ref)
		if cacheErr != nil {
			return temporary("storage", cacheErr)
		}
		if cached {
			return nil
		}
		objectLease, err := s.tasks.Leases().Acquire(ctx, "objects/"+identity, owner, objectLeaseTTL)
		if err == nil {
			if waiting {
				s.service.metrics.objectLeases.WithLabelValues("waiting").Dec()
				waiting = false
			}
			s.service.metrics.objectLeases.WithLabelValues("active").Inc()
			defer s.service.metrics.objectLeases.WithLabelValues("active").Dec()
			leaseCtx, cancel := context.WithCancel(ctx)
			renewDone := make(chan error, 1)
			go renewLease(leaseCtx, cancel, s.tasks.Leases(), objectLease, renewDone)
			if cached, cachedErr := s.cacheHas(ctx, task, ref); cachedErr != nil {
				cancel()
				<-renewDone
				_ = s.tasks.Leases().Release(context.Background(), objectLease)
				return temporary("storage", cachedErr)
			} else if cached {
				cancel()
				renewErr := <-renewDone
				_ = s.tasks.Leases().Release(context.Background(), objectLease)
				return renewErr
			}
			downloadErr := s.downloadObject(leaseCtx, task, taskLease, ref, manifest, objectLease)
			cancel()
			renewErr := <-renewDone
			_ = s.tasks.Leases().Release(context.Background(), objectLease)
			if renewErr != nil {
				return renewErr
			}
			return downloadErr
		}
		if !errors.Is(err, lease.ErrHeld) {
			return temporary("storage", err)
		}
		if !waiting {
			s.service.metrics.objectLeases.WithLabelValues("waiting").Inc()
			waiting = true
		}
		if time.Now().After(deadline) {
			s.service.metrics.recordEvent("download_wait_timeout")
			return temporary("object_wait_timeout", errors.New("timed out waiting for shared object download"))
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if cached, cacheErr := s.cacheHas(ctx, task, ref); cacheErr != nil {
			return temporary("storage", cacheErr)
		} else if cached {
			return nil
		}
	}
}

func (s *solidifier) downloadObject(ctx context.Context, task *tasks.Task, taskLease lease.Lease, ref contentRef, manifest bool, objectLease lease.Lease) error {
	endpoint := "blobs"
	limit := s.cfg.Limits.ObjectBytes
	if manifest {
		endpoint = "manifests"
		limit = s.cfg.Limits.ManifestBytes
	}
	requestPath := "/v2/" + task.Repository + "/" + endpoint + "/" + url.PathEscape(ref.digest)
	headers := make(http.Header)
	if manifest {
		headers.Set("Accept", manifestAccept)
	}
	response, err := s.retryUpstream(ctx, task.Authority, task.Repository, requestPath, nil, headers)
	if err != nil {
		return temporary("upstream", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return permanent("upstream_not_found", errors.New("referenced object does not exist upstream"))
	}
	if response.StatusCode != http.StatusOK {
		return temporary("upstream", fmt.Errorf("upstream returned status %d", response.StatusCode))
	}
	size := ref.size
	mediaType := ref.mediaType
	if manifest {
		if size >= 0 && response.ContentLength >= 0 {
			if response.ContentLength != size {
				return permanent("manifest_invalid", errors.New("manifest size does not match its descriptor"))
			}
		}
		if size == 0 || size > limit {
			return permanent("object_limit", errors.New("manifest size is invalid or exceeds limit"))
		}
		var parseErr error
		mediaType, _, parseErr = mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || mediaType == "" || ref.mediaType != "" && !sameMediaType(ref.mediaType, mediaType) {
			return permanent("manifest_invalid", errors.New("upstream manifest Content-Type is invalid"))
		}
		if digestHeader := response.Header.Get("Docker-Content-Digest"); digestHeader != "" && digestHeader != ref.digest {
			return permanent("digest_mismatch", fmt.Errorf("%w: upstream manifest digest header does not match its descriptor", artifact.ErrDigestMismatch))
		}
	} else if size < 0 || size > limit {
		return permanent("object_limit", errors.New("blob size is invalid or exceeds limit"))
	} else if response.ContentLength >= 0 && response.ContentLength != size {
		return permanent("manifest_invalid", errors.New("blob size does not match its descriptor"))
	}
	if err := s.tasks.Leases().Validate(ctx, objectLease); err != nil {
		return err
	}
	if manifest && ref.size < 0 {
		hasher, hashErr := artifact.Hasher(ref.digest)
		if hashErr != nil {
			return permanent("manifest_invalid", hashErr)
		}
		spool, actualSize, spoolErr := spoolResponse(response.Body, limit, hasher)
		if spoolErr != nil {
			return permanent("manifest_invalid", spoolErr)
		}
		defer removeSpool(spool)
		if response.ContentLength >= 0 && response.ContentLength != actualSize {
			return permanent("manifest_invalid", errors.New("manifest size does not match Content-Length"))
		}
		if artifact.DigestFromHasher(ref.digest, hasher) != ref.digest {
			return permanent("digest_mismatch", artifact.ErrDigestMismatch)
		}
		if digestHeader := response.Header.Get("Docker-Content-Digest"); digestHeader != "" && digestHeader != ref.digest {
			return permanent("digest_mismatch", fmt.Errorf("%w: manifest digest header does not match its content", artifact.ErrDigestMismatch))
		}
		ref.size = actualSize
		if _, seekErr := spool.Seek(0, io.SeekStart); seekErr != nil {
			return temporary("storage", seekErr)
		}
		if err := s.cache.PutChecked(ctx, task.Authority, task.Repository, ref.digest, mediaType, spool, actualSize, func() error {
			if err := s.tasks.Leases().Validate(ctx, objectLease); err != nil {
				return err
			}
			return s.tasks.CheckActive(ctx, task, taskLease)
		}); err != nil {
			return temporary("storage", err)
		}
		if err := s.tasks.Leases().Validate(ctx, objectLease); err != nil {
			return err
		}
		return s.checkpoint(ctx, task, taskLease, ref)
	}
	if err := s.cache.PutChecked(ctx, task.Authority, task.Repository, ref.digest, mediaType, io.LimitReader(response.Body, size+1), size, func() error {
		if err := s.tasks.Leases().Validate(ctx, objectLease); err != nil {
			return err
		}
		return s.tasks.CheckActive(ctx, task, taskLease)
	}); err != nil {
		return temporary("storage", err)
	}
	if err := s.tasks.Leases().Validate(ctx, objectLease); err != nil {
		return err
	}
	if manifest && ref.mediaType != "" && !sameMediaType(ref.mediaType, mediaType) {
		return permanent("manifest_invalid", errors.New("manifest media type does not match its descriptor"))
	}
	return s.checkpoint(ctx, task, taskLease, ref)
}

func (s *solidifier) cachePut(ctx context.Context, task *tasks.Task, taskLease lease.Lease, digest, mediaType string, body io.Reader, size int64) error {
	return s.cache.PutChecked(ctx, task.Authority, task.Repository, digest, mediaType, body, size, func() error {
		return s.tasks.CheckActive(ctx, task, taskLease)
	})
}
