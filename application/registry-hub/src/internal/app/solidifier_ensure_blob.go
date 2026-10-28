package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) ensureBlob(ctx context.Context, task *tasks.Task, taskLease lease.Lease, blob contentRef) (result error) {
	protectionSessionID := task.ProtectionSessionID
	finishOperation, err := s.beginProtectedOperation(ctx, protectionSessionID)
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
	if err := s.service.inventoryStart(ctx, task, blob.digest); err != nil {
		return err
	}
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	if response.StatusCode == http.StatusOK {
		_ = response.Body.Close()
		if response.ContentLength != blob.size || response.Header.Get("Docker-Content-Digest") != blob.digest {
			return permanent("distribution_invalid", errors.New("existing Distribution blob does not match its descriptor"))
		}
		if err := s.markRemoteContentObserved(ctx, protectionSessionID); err != nil {
			return temporary("protection_state", err)
		}
		if err := s.verifyOrReuseLocalBlob(ctx, task, blob, taskProtectionSession(task), path); err != nil {
			return err
		}
		return s.resolveRemoteWrite(ctx, protectionSessionID, remoteWrite)
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
				if err := s.confirmBlobAfterUpload(confirmCtx, task, protectionSessionID, path, blob.digest, blob.size); err == nil {
					if resolveErr := s.resolveRemoteWrite(confirmCtx, protectionSessionID, remoteWrite); resolveErr != nil {
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
	if err := s.markRemoteWrite(ctx, protectionSessionID, remoteWrite); err != nil {
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
	if err := s.markRemoteContentObserved(ctx, protectionSessionID); err != nil {
		return temporary("protection_state", err)
	}
	if err := s.verifyOrReuseLocalBlob(ctx, task, blob, taskProtectionSession(task), path); err != nil {
		return err
	}
	if err := s.resolveRemoteWrite(ctx, protectionSessionID, remoteWrite); err != nil {
		return err
	}
	return s.checkpoint(ctx, task, taskLease, blob)
}

func (s *solidifier) confirmBlobAfterUpload(ctx context.Context, task *tasks.Task, protectionSessionID, path, digest string, size int64) error {
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
	if protectionSessionID != "" {
		if err := s.markRemoteContentObserved(ctx, protectionSessionID); err != nil {
			return err
		}
	}
	return s.verifyLocalObject(ctx, path, digest, size)
}
