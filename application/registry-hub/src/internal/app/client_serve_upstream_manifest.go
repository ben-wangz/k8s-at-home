package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) serveUpstreamManifest(writer http.ResponseWriter, request *http.Request, routed route.Route, meta *requestMeta) {
	clientAccept := combinedAccept(request.Header)
	if !knownAcceptSupported(clientAccept) {
		meta.cacheSource = "upstream"
		meta.errorClass = "manifest_negotiation"
		registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no supported manifest media type was requested")
		return
	}
	operationCtx := request.Context()
	var releaseTag func() error
	if !isDigestReference(routed.Reference) {
		tagCtx, _, release, err := acquireRenewedLease(operationCtx, s.tasks.Leases(), tagLeaseKey(routed.Upstream, routed.RemoteRepo, routed.Reference), requestID(), 60*time.Second)
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "manifest state is unavailable")
			} else {
				writer.Header().Set("Retry-After", "1")
				registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "manifest tag is being modified")
			}
			return
		}
		operationCtx = tagCtx
		releaseTag = release
		defer releaseTag()
	}
	discoveryTask, _, err := s.tasks.Enqueue(operationCtx, routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err != nil {
		meta.errorClass = "queue"
		if isStorageFailure(err) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "solidification queue is unavailable")
		return
	}
	generation, err := s.generation(operationCtx, routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err != nil {
		meta.errorClass = "storage"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, meta.errorClass, err)
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "manifest generation is unavailable")
		return
	}
	if isDigestReference(routed.Reference) && discoveryTask.RootDigest != "" && discoveryTask.Generation == generation.Generation {
		cached, cacheErr := s.cache.GetAny(operationCtx, routed.Upstream, routed.RemoteRepo, discoveryTask.RootDigest)
		if cacheErr == nil {
			if acceptsMedia(clientAccept, cached.MediaType) {
				defer cached.Body.Close()
				meta.cacheSource = "cache"
				s.serveCached(writer, request, routed, cached, false)
				return
			}
			_ = cached.Body.Close()
		} else if !errors.Is(cacheErr, storage.ErrNotFound) {
			meta.errorClass = "storage"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
			return
		}
	}
	headers := requestHeaders(request.Header)
	headers.Set("Accept", manifestAcceptFor(clientAccept))
	response, err := s.fetchUpstreamMethod(operationCtx, http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, headers)
	if err != nil {
		meta.errorClass = upstreamFailureClass(err)
		meta.cacheSource = "upstream"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, "upstream", err)
		writeUpstreamError(writer, routed.Endpoint, err)
		return
	}
	if response.StatusCode == http.StatusNotModified {
		_ = response.Body.Close()
		meta.errorClass = "upstream"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, meta.errorClass, errors.New("upstream returned not modified without a cached representation"))
		copyRegistryHeaders(writer.Header(), response.Header)
		writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		meta.cacheSource = "upstream"
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	if response.StatusCode != http.StatusOK {
		meta.errorClass = "upstream"
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			meta.errorClass = "upstream_auth"
		}
		_ = response.Body.Close()
		meta.cacheSource = "upstream"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, "upstream", errors.New("upstream manifest request failed"))
		writeUpstreamStatus(writer, response.StatusCode, routed.Endpoint)
		return
	}
	data, mediaType, digest, err := readUpstreamManifest(response, routed.Reference, s.cfg.Limits.ManifestBytes)
	if err != nil {
		meta.errorClass = manifestValidationFailureClass(err)
		meta.cacheSource = "upstream"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, meta.errorClass, err)
		registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream manifest failed validation")
		return
	}
	if _, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes); err != nil {
		meta.errorClass = "manifest_invalid"
		meta.cacheSource = "upstream"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, meta.errorClass, err)
		registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream manifest failed validation")
		return
	}
	if err := s.cache.Put(operationCtx, routed.Upstream, routed.RemoteRepo, digest, mediaType, bytes.NewReader(data), int64(len(data))); err != nil {
		meta.errorClass = "storage"
		meta.cacheSource = "storage"
		s.markDiscoveryFailed(operationCtx, &discoveryTask, meta.errorClass, err)
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
		return
	}
	discoveryMediaType, discoveryDigest := mediaType, digest
	_, _, enqueueErr := s.tasks.ResolveDiscovery(operationCtx, discoveryTask.ID, routed.Upstream, routed.RemoteRepo, routed.Reference, discoveryDigest, discoveryMediaType, generation.Generation)
	if errors.Is(enqueueErr, tasks.ErrCancelled) {
		enqueueErr = nil
	}
	if enqueueErr != nil {
		meta.errorClass = "queue"
		if isStorageFailure(enqueueErr) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "solidification queue is unavailable")
		return
	}
	if !acceptsMedia(clientAccept, mediaType) {
		compatHeaders := requestHeaders(request.Header)
		compatHeaders.Set("Accept", clientAccept)
		compat, compatErr := s.fetchUpstreamMethod(operationCtx, http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, compatHeaders)
		if compatErr != nil {
			meta.errorClass = upstreamFailureClass(compatErr)
			meta.cacheSource = "upstream"
			writeUpstreamError(writer, routed.Endpoint, compatErr)
			return
		}
		if compat.StatusCode != http.StatusOK {
			meta.errorClass = "upstream"
			_ = compat.Body.Close()
			meta.cacheSource = "upstream"
			writeUpstreamStatus(writer, compat.StatusCode, routed.Endpoint)
			return
		}
		body, compatibleType, compatibleDigest, readErr := readUpstreamManifest(compat, routed.Reference, s.cfg.Limits.ManifestBytes)
		if readErr != nil {
			meta.errorClass = manifestValidationFailureClass(readErr)
			meta.cacheSource = "upstream"
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "compatible upstream manifest failed validation")
			return
		}
		if !acceptsMedia(clientAccept, compatibleType) {
			meta.errorClass = "manifest_negotiation"
			meta.cacheSource = "upstream"
			registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no compatible manifest representation is available")
			return
		}
		if _, parseErr := artifact.ParseManifest(body, compatibleType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes); parseErr != nil {
			meta.errorClass = "manifest_invalid"
			meta.cacheSource = "upstream"
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "compatible upstream manifest failed validation")
			return
		}
		if err := s.cache.Put(operationCtx, routed.Upstream, routed.RemoteRepo, compatibleDigest, compatibleType, bytes.NewReader(body), int64(len(body))); err != nil {
			meta.errorClass = "storage"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
			return
		}
		data, mediaType, digest = body, compatibleType, compatibleDigest
	}
	meta.cacheSource = "upstream"
	writeManifestBody(writer, request, routed.Repository, data, mediaType, digest)
}

func (s *Service) markDiscoveryFailed(ctx context.Context, task *tasks.Task, class string, cause error) {
	if task == nil || task.Phase != "discovery" || task.RootDigest != "" {
		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.tasks.MarkDiscoveryFailed(persistCtx, task, class, cause)
}

func (s *Service) serveUpstreamBlob(writer http.ResponseWriter, request *http.Request, routed route.Route, meta *requestMeta) {
	cached, err := s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err == nil {
		intent, intentErr := s.ensureBlobIntent(request.Context(), routed)
		if intentErr != nil {
			_ = cached.Body.Close()
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "blob solidification intent could not be recorded")
			return
		}
		if _, intentErr = s.records.SetBlobIntentReady(request.Context(), intent.ID, cached.MediaType, cached.DataVersion, cached.Size); intentErr != nil {
			s.logBlobIntentDeferred("cached blob solidification intent could not be activated", intentErr)
		}
		s.scheduleCachedBlobIntent(intent.Identity)
		defer cached.Body.Close()
		meta.cacheSource = "cache"
		s.serveCached(writer, request, routed, cached, true)
		return
	}
	if !errors.Is(err, storage.ErrNotFound) {
		meta.errorClass = "storage"
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
		return
	}
	intent, err := s.ensureBlobIntent(request.Context(), routed)
	if err != nil {
		meta.errorClass = "storage"
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "blob solidification intent could not be recorded")
		return
	}
	if err := s.cacheUpstreamBlob(request.Context(), routed); err != nil {
		var fetchErr *upstreamBlobFetchError
		if errors.As(err, &fetchErr) {
			meta.errorClass = upstreamFailureClass(fetchErr)
			writeUpstreamBlobFetchError(writer, routed.Endpoint, fetchErr)
			return
		}
		if errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrNotFound) {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "verified blob cache is unavailable")
			return
		}
		meta.errorClass = "storage"
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "upstream blob could not be cached")
		return
	}
	cached, err = s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err != nil {
		meta.errorClass = "storage"
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "verified blob cache is unavailable")
		return
	}
	defer cached.Body.Close()
	if err := s.activateBlobIntent(request.Context(), intent, cached); err != nil {
		s.logBlobIntentDeferred("blob solidification intent could not be activated", err)
	}
	meta.cacheSource = "upstream"
	s.serveCached(writer, request, routed, cached, true)
}

func (s *Service) logBlobIntentDeferred(message string, _ error) {
	if s.logger != nil {
		s.logger.Warn(message, "error_class", "storage")
	}
}

type upstreamBlobFetchError struct {
	status     int
	retryAfter string
	err        error
}

func (e *upstreamBlobFetchError) Error() string { return e.err.Error() }
func (e *upstreamBlobFetchError) Unwrap() error { return e.err }

func writeUpstreamBlobFetchError(writer http.ResponseWriter, endpoint route.Endpoint, fetchErr *upstreamBlobFetchError) {
	if fetchErr.status != 0 {
		if fetchErr.retryAfter != "" {
			writer.Header().Set("Retry-After", fetchErr.retryAfter)
		}
		writeUpstreamStatus(writer, fetchErr.status, endpoint)
		return
	}
	writeUpstreamError(writer, endpoint, fetchErr.err)
}

func (s *Service) cacheUpstreamBlob(ctx context.Context, routed route.Route) error {
	return s.cacheUpstreamBlobUntil(ctx, routed, time.Now().Add(30*time.Second))
}
