package app

import (
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/lease"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strconv"
	"strings"
)

func (s *Service) deleteLocal(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	authority, repository := routeStorageIdentity(routed)
	releasePush, ok := s.acquireSlot(request.Context(), s.pushSlots)
	if !ok {
		s.metrics.recordRejection("delete_concurrency")
		writer.Header().Set("Retry-After", "1")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "delete concurrency limit exceeded")
		return
	}
	defer releasePush()
	releaseGate, err := s.gate.Enter(request.Context())
	if err != nil {
		if isStorageFailure(err) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "Registry writes are blocked")
		return
	}
	defer releaseGate()
	reference := routed.Reference
	if routed.Endpoint == route.EndpointManifest {
		protected := false
		if strings.Contains(reference, ":") {
			protected, err = s.protectedFallbackDigest(request.Context(), routed, reference)
		} else {
			protected, err = s.protectedFallbackTag(request.Context(), routed, reference)
		}
		if err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
			return
		}
		if protected {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "fallback referrers tag is protected")
			return
		}
	}
	var tagLease lease.Lease
	deleteCtx := request.Context()
	var releaseContent func() error
	if routed.Endpoint == route.EndpointManifest && !strings.Contains(reference, ":") {
		var releaseTag func() error
		deleteCtx, tagLease, _, releaseTag, err = s.beginTagMutation(request.Context(), authority, repository, reference)
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag state is unavailable")
				return
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag is being modified")
			return
		}
		defer releaseTag()
		protected, err := s.protectedFallbackTag(deleteCtx, routed, reference)
		if err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
			return
		}
		if protected {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "fallback referrers tag is protected or its lease was lost")
			return
		}
		if err := s.tasks.Leases().Validate(deleteCtx, tagLease); err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "tag lease state is unavailable")
				return
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "tag publication lease was lost")
			return
		}
	} else {
		deleteCtx, releaseContent, err = acquireContentLease(request.Context(), s.tasks.Leases(), authority, repository, reference, requestID())
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "content state is unavailable")
				return
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "content is being modified")
			return
		}
		defer releaseContent()
		if routed.Endpoint == route.EndpointManifest {
			protected, err := s.protectedFallbackDigest(deleteCtx, routed, reference)
			if err != nil {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
				return
			}
			if protected {
				registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "fallback referrers index digest is protected")
				return
			}
		}
		if _, err := s.advanceGeneration(deleteCtx, authority, repository, reference, true); err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "deletion generation could not be advanced")
			return
		}
	}
	var referrerRepair *referrerRepairIntent
	if routed.Endpoint == route.EndpointManifest {
		referrerRepair, err = s.removeReferrerBeforeDelete(deleteCtx, routed, reference)
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
			}
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer index removal could not be confirmed")
			return
		}
	}
	response, err := s.registry.Do(deleteCtx, http.MethodDelete, registryPath(routed), query, requestHeaders(request.Header), nil, 0)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry is unavailable")
		return
	}
	if response.StatusCode == http.StatusAccepted {
		verifyPath := registryPath(routed)
		verify, verifyErr := s.registry.Do(deleteCtx, http.MethodGet, verifyPath, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
		if verifyErr != nil {
			_ = response.Body.Close()
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "delete result could not be verified")
			return
		}
		missing := verify.StatusCode == http.StatusNotFound && confirmedMiss(verify, http.MethodGet, routed.Endpoint)
		_ = verify.Body.Close()
		if !missing {
			_ = response.Body.Close()
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "deleted object is still present")
			return
		}
		if referrerRepair != nil {
			if err := s.clearReferrerRepair(deleteCtx, referrerRepair); err != nil {
				if isStorageFailure(err) {
					meta.errorClass = "storage"
				}
				_ = response.Body.Close()
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer deletion repair could not be finalized")
				return
			}
		}
	}
	meta.cacheSource = "local"
	s.writeRegistryResponse(writer, request, routed, response, false)
}

func requestedRange(value string, size int64) (int64, int64, bool, error) {
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size <= 0 {
		return 0, 0, false, errors.New("unsupported Range header")
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid Range header")
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, errors.New("invalid suffix range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("range start is unsatisfiable")
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("range end is invalid")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}

func rewritePageLink(link, repository, endpoint string) string {
	start := strings.IndexByte(link, '<')
	end := strings.IndexByte(link, '>')
	if start < 0 || end <= start {
		return ""
	}
	parsed, err := url.Parse(link[start+1 : end])
	if err != nil {
		return ""
	}
	if endpoint == "" {
		pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(pathParts) > 0 {
			endpoint = strings.Join(pathParts[len(pathParts)-2:], "/")
		}
	}
	query := parsed.Query()
	if endpoint == "_catalog" && query.Get("last") != "" {
		if logical, ok := route.DecodeInternalName(query.Get("last")); ok {
			query.Set("last", logical)
		}
	}
	requestPath := "/v2/" + repository + "/" + strings.TrimPrefix(endpoint, "/")
	if endpoint == "_catalog" {
		requestPath = "/v2/_catalog"
	}
	return "<" + requestPath + "?" + query.Encode() + ">" + link[end+1:]
}
