package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) ensureManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest contentRef, reference, protectionSessionID string) (result error) {
	finishOperation, err := s.beginProtectedOperation(ctx, protectionSessionID)
	if err != nil {
		return err
	}
	defer s.finishProtectedOperation(finishOperation, &result)
	if manifest.size < 0 || manifest.size > s.cfg.Limits.ManifestBytes {
		return permanent("manifest_invalid", errors.New("manifest size exceeds the configured limit"))
	}
	requestTimeout := s.cfg.Limits.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 300 * time.Second
	}
	manifestCtx, cancelManifest := context.WithTimeout(ctx, requestTimeout)
	defer cancelManifest()
	ctx = manifestCtx
	leaseCtx, releaseObject, err := acquireContentLease(ctx, s.tasks.Leases(), task.Authority, task.Repository, manifest.digest, task.ID)
	if errors.Is(err, lease.ErrHeld) {
		return waitForObjectLease(task.Authority, task.Repository, manifest.digest)
	}
	if err != nil {
		return temporary("object_lease", err)
	}
	defer func() {
		_ = releaseObject()
		s.service.signalTaskReconciliation()
	}()
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
	if err := s.service.inventoryStart(ctx, task, manifest.digest); err != nil {
		return err
	}
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	if response.StatusCode == http.StatusOK {
		if protectionSessionID != "" {
			if err := s.gate.MarkProtectionContentObserved(ctx, protectionSessionID); err != nil {
				_ = response.Body.Close()
				return temporary("protection_state", err)
			}
		}
		reader, hashErr := newUploadIntegrityReader(ctx, response.Body, manifest.digest, manifest.size)
		if hashErr != nil {
			_ = response.Body.Close()
			return permanent("manifest_invalid", hashErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, s.cfg.Limits.ManifestBytes+1))
		nativeSubjectHeader := response.Header.Get("OCI-Subject")
		_ = response.Body.Close()
		if readErr != nil || reader.Finalize() != nil || int64(len(data)) != manifest.size || !sameMediaType(response.Header.Get("Content-Type"), manifest.mediaType) {
			return permanent("distribution_invalid", errors.New("existing Distribution manifest does not match its digest"))
		}
		if rootDigestReference {
			if err := s.tasks.MarkRootPublished(ctx, task, taskLease); err != nil {
				return err
			}
		}
		if err := s.resolveRemoteWrite(ctx, protectionSessionID, remoteWrite); err != nil {
			return err
		}
		return s.syncReferrerManifestWithProtectionSession(ctx, task, taskLease, data, manifest.mediaType, manifest.digest, nativeSubjectHeader, false, protectionSessionID)
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
	if object.Size != manifest.size {
		return permanent("manifest_invalid", errors.New("cached manifest size differs from its descriptor"))
	}
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, manifest.digest); err != nil {
		return permanent("generation", err)
	}
	permitID, releasePermit, err := s.service.acquireProtectionWritePermit(ctx, protectionSessionID)
	if err != nil {
		return err
	}
	defer func() {
		if err := releasePermit(); err != nil {
			result = errors.Join(result, temporary("protection_write_permit", err))
		}
	}()
	if err := s.markRemoteWrite(ctx, protectionSessionID, permitID, remoteWrite); err != nil {
		return err
	}
	uploadReader, err := newUploadIntegrityReader(ctx, object.Body, manifest.digest, object.Size)
	if err != nil {
		return permanent("manifest_invalid", err)
	}
	var manifestBytes bytes.Buffer
	put, err := s.registry.Do(ctx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{manifest.mediaType}}, io.TeeReader(uploadReader, &manifestBytes), object.Size)
	if err != nil {
		return temporary("distribution", err)
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != manifest.digest {
		return temporary("distribution", fmt.Errorf("Distribution manifest PUT returned status %d", put.StatusCode))
	}
	if err := uploadReader.Finalize(); err != nil {
		return temporary("distribution_integrity", err)
	}
	nativeSubjectHeader := put.Header.Get("OCI-Subject")
	if err := s.markRemoteContentObserved(ctx, protectionSessionID); err != nil {
		return temporary("protection_state", err)
	}
	if err := s.resolveRemoteWrite(ctx, protectionSessionID, remoteWrite); err != nil {
		return err
	}
	if rootDigestReference {
		if err := s.tasks.MarkRootPublished(ctx, task, taskLease); err != nil {
			return err
		}
	}
	if err := s.syncReferrerManifestWithProtectionSession(ctx, task, taskLease, manifestBytes.Bytes(), manifest.mediaType, manifest.digest, nativeSubjectHeader, true, protectionSessionID); err != nil {
		return err
	}
	return s.checkpoint(ctx, task, taskLease, manifest)
}

func (s *solidifier) syncReferrerManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, data []byte, mediaType, digest, nativeSubjectHeader string, requireSubjectHeader bool) error {
	return s.syncReferrerManifestWithProtectionSession(ctx, task, taskLease, data, mediaType, digest, nativeSubjectHeader, requireSubjectHeader, taskProtectionSession(task))
}

func (s *solidifier) syncReferrerManifestWithProtectionSession(ctx context.Context, task *tasks.Task, taskLease lease.Lease, data []byte, mediaType, digest, nativeSubjectHeader string, requireSubjectHeader bool, protectionSessionID string) error {
	manifest, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return permanent("referrer_index", invalidReferrer("referrer manifest failed validation"))
	}
	fenced := manifest.Subject != nil && !task.RootPublished
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
	var yielded *tasks.Yield
	if errors.As(err, &yielded) {
		if fenced {
			if abortErr := s.tasks.AbortRootPublish(ctx, task, taskLease); abortErr != nil {
				return abortErr
			}
		}
		return err
	}
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
