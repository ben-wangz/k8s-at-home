package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strings"
	"time"
)

func (s *Service) acquireManifestContentLease(writer http.ResponseWriter, request *http.Request, routed route.Route, authority, repository, digest string, meta *requestMeta) (context.Context, func() error, bool) {
	if !strings.Contains(routed.Reference, ":") {
		return nil, nil, true
	}
	contentCtx, releaseContent, err := acquireContentLease(request.Context(), s.tasks.Leases(), authority, repository, digest, requestID())
	if err != nil {
		if isStorageFailure(err) {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "manifest state is unavailable")
			return nil, nil, false
		}
		registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "manifest digest is being modified")
		return nil, nil, false
	}
	if _, err := s.advanceGeneration(request.Context(), authority, repository, digest, false); err != nil {
		_ = releaseContent()
		meta.errorClass = "storage"
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "manifest generation could not be advanced")
		return nil, nil, false
	}
	return contentCtx, releaseContent, true
}

func (s *Service) validateUnprotectedManifestTags(writer http.ResponseWriter, request *http.Request, routed route.Route, tags []string, meta *requestMeta) bool {
	for _, tag := range tags {
		protected, err := s.protectedFallbackTag(request.Context(), routed, tag)
		if err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
			return false
		}
		if protected {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "fallback referrers tag is protected")
			return false
		}
	}
	return true
}

func (s *Service) acquireManifestTagLeases(writer http.ResponseWriter, request *http.Request, authority, repository string, tags []string, meta *requestMeta) ([]heldTagLease, bool) {
	leases := make([]heldTagLease, 0, len(tags))
	for _, tag := range tags {
		tagCtx, currentLease, release, err := acquireRenewedLease(request.Context(), s.tasks.Leases(), tagLeaseKey(authority, repository, tag), requestID(), 60*time.Second)
		if err != nil {
			releaseTagLeases(leases)
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag state is unavailable")
				return nil, false
			}
			writer.Header().Set("Retry-After", "1")
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag publication is already in progress")
			return nil, false
		}
		leases = append(leases, heldTagLease{tag: tag, ctx: tagCtx, lease: currentLease, release: release})
	}
	return leases, true
}

func (s *Service) validateManifestTagLeases(writer http.ResponseWriter, routed route.Route, authority, repository, digest string, leases []heldTagLease, meta *requestMeta) bool {
	for _, held := range leases {
		if err := s.tasks.Leases().Validate(held.ctx, held.lease); err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag lease state is unavailable")
				return false
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag publication lease was lost")
			return false
		}
		protected, err := s.protectedFallbackTag(held.ctx, routed, held.tag)
		if err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
			return false
		}
		if protected {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "fallback referrers tag is protected")
			return false
		}
		if _, err := s.advanceGeneration(held.ctx, authority, repository, held.tag, false); err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag generation could not be advanced")
			return false
		}
	}
	for _, held := range leases {
		if err := s.tasks.Leases().Validate(held.ctx, held.lease); err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag lease state is unavailable")
				return false
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag publication lease was lost")
			return false
		}
		if exists, err := s.checkTagConflict(held.ctx, routed, held.tag, digest); err != nil {
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "existing tag could not be verified")
			return false
		} else if exists {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag already points to a different digest")
			return false
		}
	}
	return true
}

func (s *Service) publishManifestTags(writer http.ResponseWriter, request *http.Request, routed route.Route, mediaType string, data []byte, digest string, tags []string, leases []heldTagLease, nativeSubjectHeader string) ([]string, string, bool) {
	accepted := make([]string, 0, len(tags))
	for _, tag := range tags {
		if strings.Contains(routed.Reference, ":") {
			tagLease := findTagLease(leases, tag)
			if tagLease == nil || s.tasks.Leases().Validate(tagLease.ctx, tagLease.lease) != nil {
				setAcceptedTags(writer, accepted)
				registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag publication lease was lost")
				return accepted, nativeSubjectHeader, false
			}
			tagPath := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag)
			put, err := s.registry.Do(tagLease.ctx, http.MethodPut, tagPath, nil, http.Header{"Content-Type": []string{mediaType}}, bytes.NewReader(data), int64(len(data)))
			if err != nil {
				setAcceptedTags(writer, accepted)
				registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "tag publication could not be confirmed")
				return accepted, nativeSubjectHeader, false
			}
			if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != digest {
				_ = put.Body.Close()
				setAcceptedTags(writer, accepted)
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag publication was not confirmed")
				return accepted, nativeSubjectHeader, false
			}
			if nativeSubjectHeader == "" {
				nativeSubjectHeader = put.Header.Get("OCI-Subject")
			}
			_ = put.Body.Close()
			accepted = append(accepted, tag)
			if exists, err := s.checkTagConflict(tagLease.ctx, routed, tag, digest); err != nil || exists {
				setAcceptedTags(writer, accepted)
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag publication verification failed")
				return accepted, nativeSubjectHeader, false
			}
			continue
		}
		if exists, err := s.checkTagConflict(request.Context(), routed, tag, digest); err != nil || exists {
			setAcceptedTags(writer, accepted)
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag publication verification failed")
			return accepted, nativeSubjectHeader, false
		}
		accepted = append(accepted, tag)
	}
	return accepted, nativeSubjectHeader, true
}

func setAcceptedTags(writer http.ResponseWriter, accepted []string) {
	if len(accepted) > 0 {
		writer.Header().Set("OCI-Tag", strings.Join(accepted, ","))
	}
}

func (s *Service) checkTagConflict(ctx context.Context, routed route.Route, tag, digest string) (bool, error) {
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(tag)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
		return response.Header.Get("Docker-Content-Digest") != digest, nil
	default:
		return false, fmt.Errorf("Distribution tag read returned status %d", response.StatusCode)
	}
}

func releaseTagLeases(leases []heldTagLease) {
	for index := len(leases) - 1; index >= 0; index-- {
		_ = leases[index].release()
	}
}

func findTagLease(leases []heldTagLease, tag string) *heldTagLease {
	for index := range leases {
		if leases[index].tag == tag {
			return &leases[index]
		}
	}
	return nil
}
