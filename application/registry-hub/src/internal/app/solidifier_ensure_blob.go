package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) ensureBlob(ctx context.Context, task *tasks.Task, taskLease lease.Lease, blob contentRef) (result error) {
	finishOperation, err := s.beginProtectedOperation(ctx, task)
	if err != nil {
		return err
	}
	defer s.finishProtectedOperation(finishOperation, &result)
	leaseCtx, releaseObject, err := acquireContentLease(ctx, s.tasks.Leases(), task.Authority, task.Repository, blob.digest, task.ID)
	if errors.Is(err, lease.ErrHeld) {
		return temporary("object_busy", err)
	}
	if err != nil {
		return temporary("object_lease", err)
	}
	defer releaseObject()
	ctx = leaseCtx
	transferTimeout := s.cfg.Limits.BlobTransferTimeout
	if transferTimeout <= 0 {
		transferTimeout = 24 * time.Hour
	}
	transferCtx, cancelTransfer := context.WithTimeout(ctx, transferTimeout)
	defer cancelTransfer()
	ctx = transferCtx
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, blob.digest); err != nil {
		return permanent("generation", err)
	}
	internalRepository := s.internalName(task.Authority, task.Repository)
	path := "/v2/" + internalRepository + "/blobs/" + blob.digest
	remoteWrite := protectionRemoteWrite{Kind: "blob", Path: path, Digest: blob.digest, Size: blob.size, MediaType: "application/octet-stream"}
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	if response.StatusCode == http.StatusOK {
		_ = response.Body.Close()
		if response.ContentLength != blob.size || response.Header.Get("Docker-Content-Digest") != blob.digest {
			return permanent("distribution_invalid", errors.New("existing Distribution blob does not match its descriptor"))
		}
		if err := s.markRemoteContentObserved(ctx, task); err != nil {
			return temporary("protection_state", err)
		}
		if err := s.verifyOrReuseLocalBlob(ctx, task, blob, taskProtectionSession(task), path); err != nil {
			return err
		}
		return s.resolveRemoteWrite(ctx, task, remoteWrite)
	}
	if response.StatusCode != http.StatusNotFound {
		_ = response.Body.Close()
		return temporary("distribution", fmt.Errorf("Distribution blob HEAD returned status %d", response.StatusCode))
	}
	if !confirmedMiss(response, http.MethodHead, route.EndpointBlob) {
		_ = response.Body.Close()
		return temporary("distribution", errors.New("Distribution returned an untrusted blob miss"))
	}
	_ = response.Body.Close()
	if err := s.reserveBlobCacheInput(ctx, task, taskLease); err != nil {
		return err
	}
	object, err := s.cache.GetAnyRetained(ctx, task.Authority, task.Repository, blob.digest)
	if errors.Is(err, storage.ErrNotFound) {
		if releaseErr := s.releaseProtectedCacheReservation(ctx, task); releaseErr != nil {
			return temporary("protected_cache_reservation", releaseErr)
		}
		intent, intentErr := s.service.records.GetBlobIntent(ctx, task.ID)
		if intentErr != nil {
			return temporary("task_state", intentErr)
		}
		if releaseErr := s.releaseEmptyDirectBlobProtection(ctx, task, taskLease, intent); releaseErr != nil {
			return releaseErr
		}
		return tasks.WaitFor(tasks.WaitingCache, "cache_entry_unavailable")
	}
	if err != nil {
		return temporary("storage", err)
	}
	defer object.Body.Close()
	if object.Size != blob.size || object.DataVersion != task.CacheVersion {
		if releaseErr := s.releaseProtectedCacheReservation(ctx, task); releaseErr != nil {
			return temporary("protected_cache_reservation", releaseErr)
		}
		return permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
	}
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, blob.digest); err != nil {
		return permanent("generation", err)
	}
	uploadPath := "/v2/" + internalRepository + "/blobs/uploads/"
	create, err := s.registry.Do(ctx, http.MethodPost, uploadPath, nil, make(http.Header), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	initialLocation := create.Header.Get("Location")
	_ = create.Body.Close()
	if create.StatusCode != http.StatusAccepted {
		return temporary("distribution", fmt.Errorf("Distribution upload creation returned status %d", create.StatusCode))
	}
	uploadCommitted := false
	writeStarted := false
	defer func() {
		if !uploadCommitted {
			s.service.abortInternalUpload(initialLocation, internalRepository)
			if writeStarted {
				confirmCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Limits.RequestTimeout)
				defer cancel()
				if err := s.confirmBlobAfterUpload(confirmCtx, task, path, blob.digest, blob.size); err == nil {
					if resolveErr := s.resolveRemoteWrite(confirmCtx, task, remoteWrite); resolveErr != nil {
						result = errors.Join(result, resolveErr)
					}
				}
			}
		}
	}()
	locationPath, query, err := s.registry.ParseLocation(initialLocation, internalRepository)
	if err != nil {
		return permanent("distribution_invalid", err)
	}
	query.Set("digest", blob.digest)
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.markRemoteWrite(ctx, task, remoteWrite); err != nil {
		return err
	}
	writeStarted = true
	put, err := s.registry.Do(ctx, http.MethodPut, locationPath, query, http.Header{"Content-Type": []string{"application/octet-stream"}}, object.Body, object.Size)
	if err != nil {
		return temporary("distribution", err)
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != blob.digest {
		return temporary("distribution", fmt.Errorf("Distribution blob commit returned status %d", put.StatusCode))
	}
	uploadCommitted = true
	if err := s.markRemoteContentObserved(ctx, task); err != nil {
		return temporary("protection_state", err)
	}
	if err := s.verifyOrReuseLocalBlob(ctx, task, blob, taskProtectionSession(task), path); err != nil {
		return err
	}
	if err := s.resolveRemoteWrite(ctx, task, remoteWrite); err != nil {
		return err
	}
	return s.checkpoint(ctx, task, taskLease, blob)
}

func (s *solidifier) confirmBlobAfterUpload(ctx context.Context, task *tasks.Task, path, digest string, size int64) error {
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errors.New("post-cancellation blob is not visible yet")
	}
	if response.StatusCode != http.StatusOK || response.ContentLength != size || response.Header.Get("Docker-Content-Digest") != digest {
		return errors.New("post-cancellation blob state could not be verified")
	}
	if task.ProtectionSessionID != "" {
		if err := s.markRemoteContentObserved(ctx, task); err != nil {
			return err
		}
	}
	return s.verifyLocalObject(ctx, path, digest, size)
}

func (s *solidifier) ensureManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest contentRef, reference string) (result error) {
	finishOperation, err := s.beginProtectedOperation(ctx, task)
	if err != nil {
		return err
	}
	defer s.finishProtectedOperation(finishOperation, &result)
	requestTimeout := s.cfg.Limits.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 300 * time.Second
	}
	manifestCtx, cancelManifest := context.WithTimeout(ctx, requestTimeout)
	defer cancelManifest()
	ctx = manifestCtx
	leaseCtx, releaseObject, err := acquireContentLease(ctx, s.tasks.Leases(), task.Authority, task.Repository, manifest.digest, task.ID)
	if errors.Is(err, lease.ErrHeld) {
		return temporary("object_busy", err)
	}
	if err != nil {
		return temporary("object_lease", err)
	}
	defer releaseObject()
	ctx = leaseCtx
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, manifest.digest); err != nil {
		return permanent("generation", err)
	}
	rootDigestReference := manifest.digest == task.RootDigest && isDigestReference(task.Reference)
	if rootDigestReference {
		if err := s.tasks.BeginRootPublish(ctx, task, taskLease); err != nil {
			return err
		}
	}
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/manifests/" + url.PathEscape(reference)
	remoteWrite := protectionRemoteWrite{Kind: "manifest", Path: path, Digest: manifest.digest, Size: manifest.size, MediaType: manifest.mediaType}
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	if response.StatusCode == http.StatusOK {
		if err := s.gate.MarkProtectionContentObserved(ctx, taskProtectionSession(task)); err != nil {
			_ = response.Body.Close()
			return temporary("protection_state", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
		nativeSubjectHeader := response.Header.Get("OCI-Subject")
		_ = response.Body.Close()
		if readErr != nil || int64(len(data)) != manifest.size || artifact.VerifyDigest(manifest.digest, data) != nil || !sameMediaType(response.Header.Get("Content-Type"), manifest.mediaType) {
			return permanent("distribution_invalid", errors.New("existing Distribution manifest does not match its digest"))
		}
		if rootDigestReference {
			if err := s.tasks.MarkRootPublished(ctx, task, taskLease); err != nil {
				return err
			}
		}
		if err := s.resolveRemoteWrite(ctx, task, remoteWrite); err != nil {
			return err
		}
		return s.syncReferrerManifest(ctx, task, taskLease, data, manifest.mediaType, manifest.digest, nativeSubjectHeader, false)
	}
	if response.StatusCode != http.StatusNotFound {
		_ = response.Body.Close()
		return temporary("distribution", fmt.Errorf("Distribution manifest GET returned status %d", response.StatusCode))
	}
	if !confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
		_ = response.Body.Close()
		return temporary("distribution", errors.New("Distribution returned an untrusted manifest miss"))
	}
	_ = response.Body.Close()
	object, err := s.cache.GetRetained(ctx, task.Authority, task.Repository, manifest.digest, manifest.mediaType)
	if err != nil {
		return temporary("storage", err)
	}
	defer object.Body.Close()
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, manifest.digest); err != nil {
		return permanent("generation", err)
	}
	if err := s.markRemoteWrite(ctx, task, remoteWrite); err != nil {
		return err
	}
	put, err := s.registry.Do(ctx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{manifest.mediaType}}, object.Body, object.Size)
	if err != nil {
		return temporary("distribution", err)
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != manifest.digest {
		return temporary("distribution", fmt.Errorf("Distribution manifest PUT returned status %d", put.StatusCode))
	}
	nativeSubjectHeader := put.Header.Get("OCI-Subject")
	if err := s.markRemoteContentObserved(ctx, task); err != nil {
		return temporary("protection_state", err)
	}
	if err := s.verifyLocalObject(ctx, path, manifest.digest, manifest.size); err != nil {
		return err
	}
	if err := s.resolveRemoteWrite(ctx, task, remoteWrite); err != nil {
		return err
	}
	if rootDigestReference {
		if err := s.tasks.MarkRootPublished(ctx, task, taskLease); err != nil {
			return err
		}
	}
	data, mediaType, err := readCachedManifest(ctx, s.cache, task.Authority, task.Repository, manifest.digest, s.cfg.Limits.ManifestBytes)
	if err != nil {
		return temporary("storage", err)
	}
	if err := s.syncReferrerManifest(ctx, task, taskLease, data, mediaType, manifest.digest, nativeSubjectHeader, true); err != nil {
		return err
	}
	return s.checkpoint(ctx, task, taskLease, manifest)
}

func (s *solidifier) syncReferrerManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, data []byte, mediaType, digest, nativeSubjectHeader string, requireSubjectHeader bool) error {
	manifest, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return permanent("referrer_index", invalidReferrer("referrer manifest failed validation"))
	}
	fenced := manifest.Subject != nil && !task.RootPublished
	protectionSessionID := taskProtectionSession(task)
	if fenced {
		if err := s.tasks.BeginRootPublish(ctx, task, taskLease); err != nil {
			return err
		}
	}
	routed, err := taskRoute(task.Authority, task.Repository)
	if err != nil {
		if fenced {
			if abortErr := s.tasks.AbortRootPublish(ctx, task, taskLease); abortErr != nil {
				return abortErr
			}
		}
		return permanent("route_invalid", err)
	}
	_, err = s.service.publishReferrerManifest(ctx, routed, data, mediaType, digest, nativeSubjectHeader, requireSubjectHeader, task.ID, protectionSessionID)
	var repairPending *referrerRepairPendingError
	if fenced && !errors.As(err, &repairPending) {
		if abortErr := s.tasks.AbortRootPublish(ctx, task, taskLease); abortErr != nil {
			return abortErr
		}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNativeSubjectHeader) {
		return permanent("distribution_protocol", err)
	}
	if errors.As(err, &repairPending) {
		return &tasks.Failure{Class: "referrer_index", RepairPending: true, Err: err}
	}
	if !referrerRetryable(err) {
		return permanent("referrer_index", err)
	}
	return temporary("referrer_index", err)
}
