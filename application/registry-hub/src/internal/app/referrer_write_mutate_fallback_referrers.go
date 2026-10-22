package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) resolveProtectedReferrerWrite(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	if err := s.gate.MarkProtectionContentObserved(ctx, sessionID); err != nil {
		return err
	}
	return s.gate.MarkProtectionWriteResolved(ctx, sessionID, remoteWrite)
}

func (s *Service) mutateFallbackReferrers(ctx context.Context, routed route.Route, subject string, descriptor json.RawMessage, remove, deleteManifest bool, taskID, protectionSessionID string) (*referrerRepairIntent, error) {
	var target referrerDescriptor
	if json.Unmarshal(descriptor, &target) != nil || !route.ValidDigest(target.Digest) {
		return nil, errors.New("referrer descriptor identity is invalid")
	}
	releaseGate, err := s.gate.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseGate()
	leaseCtx, current, release, err := s.acquireReferrerLease(ctx, routed, subject)
	if err != nil {
		return nil, err
	}
	defer release()
	tag := referrerFallbackTag(subject)
	authority, repository := routeStorageIdentity(routed)
	tagCtx, tagLease, releaseTagLease, err := acquireRenewedLease(leaseCtx, s.tasks.Leases(), tagLeaseKey(authority, repository, tag), requestID(), 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer releaseTagLease()
	operation := "add"
	if remove {
		operation = "remove"
	}
	var remoteWrite *protectionRemoteWrite
	if !remove && protectionSessionID != "" {
		remoteWrite = &protectionRemoteWrite{
			Kind:         "referrer",
			Path:         "/v2/" + routed.Internal + "/referrers/" + url.PathEscape(subject),
			FallbackPath: "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag),
			Digest:       target.Digest,
			Subject:      subject,
			MediaType:    referrerIndexMediaType,
		}
	}
	intent, err := s.beginReferrerRepair(leaseCtx, routed, subject, target.Digest, operation, deleteManifest, taskID)
	if err != nil {
		return nil, fmt.Errorf("begin fallback referrer repair: %w", err)
	}
	mutate := func(current []json.RawMessage) ([]json.RawMessage, bool, error) {
		return updateReferrerDescriptors(current, descriptor, remove)
	}
	if remoteWrite == nil {
		err = s.writeFallbackReferrers(leaseCtx, current, tagCtx, tagLease, routed, subject, tag, mutate)
	} else {
		err = s.writeFallbackReferrersProtected(leaseCtx, current, tagCtx, tagLease, routed, subject, tag, mutate, remoteWrite, protectionSessionID)
	}
	if err != nil {
		if updateErr := s.recordReferrerRepairFailure(leaseCtx, intent, err); updateErr != nil {
			s.logger.Warn("referrer repair intent update failed", "error_class", "storage")
		}
		s.logger.Warn("fallback referrer index update failed", "error_class", referrerErrorClass(err), "retryable", referrerRetryable(err))
		return intent, &referrerRepairPendingError{cause: fmt.Errorf("write fallback referrer index: %w", err)}
	}
	if deleteManifest {
		return intent, nil
	}
	if err := s.clearReferrerRepair(leaseCtx, intent); err != nil {
		s.logger.Warn("completed referrer repair intent could not be removed", "error_class", "storage")
		return intent, &referrerRepairPendingError{cause: fmt.Errorf("clear completed fallback referrer repair: %w", err)}
	}
	return nil, nil
}

func (s *Service) writeFallbackReferrers(leaseCtx context.Context, current lease.Lease, tagCtx context.Context, tagLease lease.Lease, routed route.Route, subject, tag string, mutate func([]json.RawMessage) ([]json.RawMessage, bool, error), allowCorrupt ...bool) error {
	return s.writeFallbackReferrersProtected(leaseCtx, current, tagCtx, tagLease, routed, subject, tag, mutate, nil, "", allowCorrupt...)
}

func (s *Service) writeFallbackReferrersProtected(leaseCtx context.Context, current lease.Lease, tagCtx context.Context, tagLease lease.Lease, routed route.Route, subject, tag string, mutate func([]json.RawMessage) ([]json.RawMessage, bool, error), remoteWrite *protectionRemoteWrite, sessionID string, allowCorrupt ...bool) error {
	for attempt := 0; attempt < 5; attempt++ {
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			return fmt.Errorf("validate referrer lease: %w", err)
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			return fmt.Errorf("validate fallback tag lease: %w", err)
		}
		index, err := s.readFallbackIndex(tagCtx, routed, subject, tag)
		if err != nil {
			var permanent *referrerPermanentError
			if len(allowCorrupt) == 0 || !allowCorrupt[0] || !errors.As(err, &permanent) {
				return fmt.Errorf("read fallback referrer index: %w", err)
			}
			index = referrerIndex{manifests: []json.RawMessage{}}
		}
		next, changed, err := mutate(index.manifests)
		if err != nil {
			return fmt.Errorf("mutate fallback referrer descriptors: %w", err)
		}
		if remoteWrite != nil && referrerIndexContains(index, remoteWrite.Digest) {
			if err := s.resolveProtectedReferrerWrite(leaseCtx, sessionID, *remoteWrite); err != nil {
				return fmt.Errorf("resolve existing protected referrer write: %w", err)
			}
		}
		if !changed {
			if index.digest != "" {
				if err := s.recordFallbackMarker(leaseCtx, routed, subject, tag, index.digest); err != nil {
					return fmt.Errorf("record unchanged fallback referrer marker: %w", err)
				}
			}
			return nil
		}
		data, err := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: next})
		if err != nil {
			return fmt.Errorf("encode fallback referrer index: %w", err)
		}
		digest := artifact.CalculateSHA256(data)
		authority, repository := routeStorageIdentity(routed)
		indexCtx, releaseIndex, err := acquireContentLease(tagCtx, s.tasks.Leases(), authority, repository, digest, requestID())
		if err != nil {
			return fmt.Errorf("acquire fallback content lease: %w", err)
		}
		marker := referrerFallbackMarker{SchemaVersion: 1, Authority: authority, Repository: repository, Subject: subject, Tag: tag, Digest: digest}
		if err := s.storeDigestMarker(indexCtx, fallbackDigestMarkerKey(routed, digest), marker); err != nil {
			_ = releaseIndex()
			return fmt.Errorf("store fallback digest marker: %w", err)
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			_ = releaseIndex()
			return fmt.Errorf("revalidate fallback tag lease: %w", err)
		}
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			_ = releaseIndex()
			return fmt.Errorf("revalidate referrer lease: %w", err)
		}
		path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag)
		if remoteWrite != nil {
			if err := s.gate.MarkProtectionWriteStarted(indexCtx, sessionID, *remoteWrite); err != nil {
				_ = releaseIndex()
				return fmt.Errorf("record fallback referrer write intent: %w", err)
			}
		}
		put, err := s.registry.Do(indexCtx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{referrerIndexMediaType}}, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			_ = releaseIndex()
			return fmt.Errorf("put fallback referrer index: %w", err)
		}
		status, returnedDigest := put.StatusCode, put.Header.Get("Docker-Content-Digest")
		_ = put.Body.Close()
		if status != http.StatusCreated || returnedDigest != digest {
			if status == http.StatusConflict || status == http.StatusPreconditionFailed {
				if remoteWrite != nil {
					if resolveErr := s.gate.MarkProtectionWriteResolved(leaseCtx, sessionID, *remoteWrite); resolveErr != nil {
						_ = releaseIndex()
						return resolveErr
					}
				}
				if releaseErr := releaseIndex(); releaseErr != nil {
					return fmt.Errorf("release fallback content lease after conflict: %w", releaseErr)
				}
				continue
			}
			_ = releaseIndex()
			return registryStatusError("fallback referrers index PUT", status)
		}
		verified, err := s.registry.Do(indexCtx, http.MethodGet, path, nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
		if err != nil {
			_ = releaseIndex()
			return fmt.Errorf("read back fallback referrer index: %w", err)
		}
		readBack, err := s.readReferrerIndex(verified)
		if err != nil {
			_ = releaseIndex()
			return fmt.Errorf("validate fallback referrer readback: %w", err)
		}
		releaseErr := releaseIndex()
		if releaseErr != nil {
			return fmt.Errorf("release fallback content lease: %w", releaseErr)
		}
		if readBack.digest != digest {
			continue
		}
		if remoteWrite != nil {
			if !referrerIndexContains(readBack, remoteWrite.Digest) {
				return errors.New("verified fallback referrers index does not contain the expected referrer")
			}
			if err := s.gate.MarkProtectionContentObserved(leaseCtx, sessionID); err != nil {
				return fmt.Errorf("record observed fallback referrer content: %w", err)
			}
			if err := s.gate.MarkProtectionWriteResolved(leaseCtx, sessionID, *remoteWrite); err != nil {
				return fmt.Errorf("resolve fallback referrer write: %w", err)
			}
		}
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			return fmt.Errorf("final referrer lease validation: %w", err)
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			return fmt.Errorf("final fallback tag lease validation: %w", err)
		}
		if err := s.recordFallbackMarker(tagCtx, routed, subject, tag, digest); err != nil {
			return fmt.Errorf("record committed fallback referrer marker: %w", err)
		}
		return nil
	}
	return storage.ErrConditionFailed
}
