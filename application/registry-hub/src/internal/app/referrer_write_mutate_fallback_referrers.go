package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

func (s *Service) mutateFallbackReferrers(ctx context.Context, routed route.Route, subject string, descriptor json.RawMessage, remove, deleteManifest bool, taskID string) (*referrerRepairIntent, error) {
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
	intent, err := s.beginReferrerRepair(leaseCtx, routed, subject, target.Digest, operation, deleteManifest, taskID)
	if err != nil {
		return nil, err
	}
	err = s.writeFallbackReferrers(leaseCtx, current, tagCtx, tagLease, routed, subject, tag, func(current []json.RawMessage) ([]json.RawMessage, bool, error) {
		return updateReferrerDescriptors(current, descriptor, remove)
	})
	if err != nil {
		if updateErr := s.recordReferrerRepairFailure(leaseCtx, intent, err); updateErr != nil {
			s.logger.Warn("referrer repair intent update failed", "error_class", "storage")
		}
		s.logger.Warn("fallback referrer index update failed", "error_class", referrerErrorClass(err), "retryable", referrerRetryable(err))
		return intent, &referrerRepairPendingError{cause: err}
	}
	if deleteManifest {
		return intent, nil
	}
	if err := s.clearReferrerRepair(leaseCtx, intent); err != nil {
		s.logger.Warn("completed referrer repair intent could not be removed", "error_class", "storage")
		return intent, &referrerRepairPendingError{cause: err}
	}
	return nil, nil
}

func (s *Service) writeFallbackReferrers(leaseCtx context.Context, current lease.Lease, tagCtx context.Context, tagLease lease.Lease, routed route.Route, subject, tag string, mutate func([]json.RawMessage) ([]json.RawMessage, bool, error), allowCorrupt ...bool) error {
	for attempt := 0; attempt < 5; attempt++ {
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			return err
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			return err
		}
		index, err := s.readFallbackIndex(tagCtx, routed, subject, tag)
		if err != nil {
			var permanent *referrerPermanentError
			if len(allowCorrupt) == 0 || !allowCorrupt[0] || !errors.As(err, &permanent) {
				return err
			}
			index = referrerIndex{manifests: []json.RawMessage{}}
		}
		next, changed, err := mutate(index.manifests)
		if err != nil {
			return err
		}
		if !changed {
			if index.digest != "" {
				return s.recordFallbackMarker(leaseCtx, routed, subject, tag, index.digest)
			}
			return nil
		}
		data, err := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: next})
		if err != nil {
			return err
		}
		digest := artifact.CalculateSHA256(data)
		authority, repository := routeStorageIdentity(routed)
		indexCtx, releaseIndex, err := acquireContentLease(tagCtx, s.tasks.Leases(), authority, repository, digest, requestID())
		if err != nil {
			return err
		}
		marker := referrerFallbackMarker{SchemaVersion: 1, Authority: authority, Repository: repository, Subject: subject, Tag: tag, Digest: digest}
		if err := s.storeDigestMarker(indexCtx, fallbackDigestMarkerKey(routed, digest), marker); err != nil {
			_ = releaseIndex()
			return err
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			_ = releaseIndex()
			return err
		}
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			_ = releaseIndex()
			return err
		}
		path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag)
		put, err := s.registry.Do(indexCtx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{referrerIndexMediaType}}, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			_ = releaseIndex()
			return err
		}
		status, returnedDigest := put.StatusCode, put.Header.Get("Docker-Content-Digest")
		_ = put.Body.Close()
		if status != http.StatusCreated || returnedDigest != digest {
			_ = releaseIndex()
			if status == http.StatusConflict || status == http.StatusPreconditionFailed {
				continue
			}
			return registryStatusError("fallback referrers index PUT", status)
		}
		verified, err := s.registry.Do(indexCtx, http.MethodGet, path, nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
		if err != nil {
			_ = releaseIndex()
			return err
		}
		readBack, err := s.readReferrerIndex(verified)
		if err != nil {
			_ = releaseIndex()
			return err
		}
		releaseErr := releaseIndex()
		if releaseErr != nil {
			return releaseErr
		}
		if readBack.digest != digest {
			continue
		}
		if err := s.tasks.Leases().Validate(leaseCtx, current); err != nil {
			return err
		}
		if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
			return err
		}
		return s.recordFallbackMarker(tagCtx, routed, subject, tag, digest)
	}
	return storage.ErrConditionFailed
}

func updateReferrerDescriptors(current []json.RawMessage, descriptor json.RawMessage, remove bool) ([]json.RawMessage, bool, error) {
	desired := make(map[string]json.RawMessage, len(current)+1)
	for _, raw := range current {
		var item referrerDescriptor
		if json.Unmarshal(raw, &item) != nil || !route.ValidDigest(item.Digest) {
			return nil, false, invalidReferrer("fallback referrers descriptor is invalid")
		}
		canonical, err := canonicalDescriptor(raw)
		if err != nil {
			return nil, false, invalidReferrer("fallback referrers descriptor is invalid")
		}
		if existing, found := desired[item.Digest]; found && !bytes.Equal(existing, canonical) {
			return nil, false, invalidReferrer("fallback referrers index contains conflicting duplicate descriptors")
		}
		desired[item.Digest] = canonical
	}
	var target referrerDescriptor
	if len(descriptor) > 0 && (json.Unmarshal(descriptor, &target) != nil || !route.ValidDigest(target.Digest)) {
		return nil, false, invalidReferrer("new fallback referrer descriptor is invalid")
	}
	if remove {
		if len(descriptor) == 0 {
			return nil, false, invalidReferrer("remove operation requires a referrer digest")
		}
		if _, found := desired[target.Digest]; !found {
			return sortedReferrerDescriptors(desired), false, nil
		}
		delete(desired, target.Digest)
		return sortedReferrerDescriptors(desired), true, nil
	}
	descriptor, err := canonicalDescriptor(descriptor)
	if err != nil {
		return nil, false, invalidReferrer("new fallback referrer descriptor is invalid")
	}
	if existing, found := desired[target.Digest]; found {
		if !bytes.Equal(existing, descriptor) {
			return nil, false, invalidReferrer("fallback referrer digest already has conflicting metadata")
		}
		return sortedReferrerDescriptors(desired), false, nil
	}
	desired[target.Digest] = descriptor
	return sortedReferrerDescriptors(desired), true, nil
}

func canonicalDescriptor(raw json.RawMessage) (json.RawMessage, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("referrer descriptor JSON is invalid")
	}
	canonical, err := json.Marshal(value)
	return canonical, err
}

func sortedReferrerDescriptors(values map[string]json.RawMessage) []json.RawMessage {
	digests := make([]string, 0, len(values))
	for digest := range values {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	result := make([]json.RawMessage, 0, len(digests))
	for _, digest := range digests {
		result = append(result, values[digest])
	}
	return result
}
