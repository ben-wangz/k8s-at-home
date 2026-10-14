package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) readFallbackIndex(ctx context.Context, routed route.Route, subject, tag string) (referrerIndex, error) {
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return referrerIndex{}, err
	}
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
		_ = response.Body.Close()
		return referrerIndex{manifests: []json.RawMessage{}}, nil
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return referrerIndex{}, registryStatusError("fallback referrers index GET", status)
	}
	index, err := s.readReferrerIndex(response)
	if err != nil {
		return referrerIndex{}, err
	}
	if err := s.recordFallbackMarker(ctx, routed, subject, tag, index.digest); err != nil {
		return referrerIndex{}, err
	}
	return index, nil
}

func (s *Service) acquireReferrerLease(ctx context.Context, routed route.Route, subject string) (context.Context, lease.Lease, func() error, error) {
	authority, repository := routeStorageIdentity(routed)
	return acquireRenewedLease(ctx, s.tasks.Leases(), "referrers/"+objectLeaseKey(authority, repository, subject), requestID(), 60*time.Second)
}

func (s *Service) recordFallbackMarker(ctx context.Context, routed route.Route, subject, tag, digest string) error {
	authority, repository := routeStorageIdentity(routed)
	marker := referrerFallbackMarker{SchemaVersion: 1, Authority: authority, Repository: repository, Subject: subject, Tag: tag, Digest: digest}
	previous, previousErr := s.readMarker(ctx, fallbackTagMarkerKey(routed, tag), routed, tag)
	if previousErr != nil && !errors.Is(previousErr, storage.ErrNotFound) {
		return previousErr
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := s.storeDigestMarker(ctx, fallbackDigestMarkerKey(routed, digest), marker); err != nil {
		return err
	}
	if err := s.storeMarker(ctx, fallbackTagMarkerKey(routed, tag), data, marker); err != nil {
		return err
	}
	if previousErr == nil && previous.Digest != digest {
		return s.removeDigestMarker(ctx, fallbackDigestMarkerKey(routed, previous.Digest), previous)
	}
	return nil
}

func (s *Service) storeMarker(ctx context.Context, key string, data []byte, expected referrerFallbackMarker) error {
	for attempt := 0; attempt < 8; attempt++ {
		object, err := s.store.Get(ctx, key)
		options := storage.PutOptions{IfAbsent: errors.Is(err, storage.ErrNotFound)}
		if err == nil {
			previous, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
			_ = object.Body.Close()
			var current referrerFallbackMarker
			if readErr != nil || json.Unmarshal(previous, &current) != nil || current.SchemaVersion != 1 || current.Authority != expected.Authority || current.Repository != expected.Repository || current.Tag != expected.Tag || current.Subject != expected.Subject {
				return invalidReferrer("fallback referrer protection state is invalid")
			}
			if current.Digest == expected.Digest {
				return nil
			}
			options.IfMatch = object.Version
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		return err
	}
	return storage.ErrConditionFailed
}

func (s *Service) protectedFallbackTag(ctx context.Context, routed route.Route, tag string) (bool, error) {
	marker, err := s.readMarker(ctx, fallbackTagMarkerKey(routed, tag), routed, tag)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil && marker.Tag == tag, err
}

func (s *Service) protectedFallbackDigest(ctx context.Context, routed route.Route, digest string) (bool, error) {
	markers, err := s.readDigestMarkers(ctx, fallbackDigestMarkerKey(routed, digest), routed, digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, marker := range markers {
		_, readErr := s.readMarker(ctx, fallbackTagMarkerKey(routed, marker.Tag), routed, marker.Tag)
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return false, readErr
		}
		path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(marker.Tag)
		response, getErr := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
		if getErr != nil {
			return false, getErr
		}
		if response.StatusCode == http.StatusOK {
			isCurrent := response.Header.Get("Docker-Content-Digest") == digest
			_ = response.Body.Close()
			if isCurrent {
				return true, nil
			}
			continue
		}
		if response.StatusCode != http.StatusNotFound || !confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
			status := response.StatusCode
			_ = response.Body.Close()
			return false, fmt.Errorf("protected fallback tag GET returned status %d", status)
		}
		_ = response.Body.Close()
	}
	return false, nil
}

func (s *Service) protectedFallbackManifest(ctx context.Context, routed route.Route, reference string) (bool, error) {
	if strings.Contains(reference, ":") {
		return s.protectedFallbackDigest(ctx, routed, reference)
	}
	return s.protectedFallbackTag(ctx, routed, reference)
}

func (s *Service) storeDigestMarker(ctx context.Context, key string, marker referrerFallbackMarker) error {
	for attempt := 0; attempt < 8; attempt++ {
		var current []referrerFallbackMarker
		object, err := s.store.Get(ctx, key)
		options := storage.PutOptions{IfAbsent: errors.Is(err, storage.ErrNotFound)}
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(object.Body, 4<<20+1))
			_ = object.Body.Close()
			if readErr != nil || len(data) > 4<<20 || json.Unmarshal(data, &current) != nil {
				return invalidReferrer("fallback digest protection state is invalid")
			}
			options.IfMatch = object.Version
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		found := false
		for index := range current {
			item := current[index]
			if item.SchemaVersion != 1 || item.Authority != marker.Authority || item.Repository != marker.Repository || !route.ValidDigest(item.Subject) || item.Tag != referrerFallbackTag(item.Subject) || !route.ValidDigest(item.Digest) {
				return invalidReferrer("fallback digest protection entry is invalid")
			}
			if item.Tag == marker.Tag && item.Subject == marker.Subject {
				if item == marker {
					return nil
				}
				current[index] = marker
				found = true
			}
		}
		if !found {
			if len(current) >= 4096 {
				return invalidReferrer("fallback digest protection entry limit reached")
			}
			current = append(current, marker)
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		return err
	}
	return storage.ErrConditionFailed
}

func (s *Service) removeDigestMarker(ctx context.Context, key string, marker referrerFallbackMarker) error {
	for attempt := 0; attempt < 8; attempt++ {
		object, err := s.store.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 4<<20+1))
		_ = object.Body.Close()
		var current []referrerFallbackMarker
		if readErr != nil || len(data) > 4<<20 || json.Unmarshal(data, &current) != nil {
			return invalidReferrer("fallback digest protection state is invalid")
		}
		filtered := current[:0]
		removed := false
		for _, item := range current {
			if item.Authority == marker.Authority && item.Repository == marker.Repository && item.Subject == marker.Subject && item.Tag == marker.Tag && item.Digest == marker.Digest {
				removed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !removed {
			return nil
		}
		if len(filtered) == 0 {
			if err := s.store.DeleteIfMatch(ctx, key, object.Version); errors.Is(err, storage.ErrConditionFailed) {
				continue
			} else if errors.Is(err, storage.ErrNotFound) {
				return nil
			} else {
				return err
			}
		}
		updated, err := json.Marshal(filtered)
		if err != nil {
			return err
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
		if errors.Is(err, storage.ErrConditionFailed) {
			continue
		}
		return err
	}
	return storage.ErrConditionFailed
}
