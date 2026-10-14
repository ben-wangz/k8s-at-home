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
	if !knownAcceptSupported(request.Header.Get("Accept")) {
		meta.cacheSource = "upstream"
		registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no supported manifest media type was requested")
		return
	}
	var task tasks.Task
	var created bool
	var err error
	if routed.Upstream != "" {
		task, created, err = s.tasks.Enqueue(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
		if err != nil {
			meta.errorClass = "queue"
			if isStorageFailure(err) {
				meta.errorClass = "storage"
			}
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "solidification queue is unavailable")
			return
		}
	}
	if task.RootDigest != "" {
		if cached, cacheErr := s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, task.RootDigest); cacheErr == nil {
			defer cached.Body.Close()
			if acceptsMedia(request.Header.Get("Accept"), cached.MediaType) {
				meta.cacheSource = "cache"
				s.serveCached(writer, request, routed, cached, false)
				return
			}
		} else if !errors.Is(cacheErr, storage.ErrNotFound) {
			meta.errorClass = "storage"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
			return
		}
	}
	headers := requestHeaders(request.Header)
	headers.Set("Accept", manifestAcceptFor(request.Header.Get("Accept")))
	response, err := s.fetchUpstreamMethod(request.Context(), http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, headers)
	if err != nil {
		meta.errorClass = upstreamFailureClass(err)
		if created {
			s.markDiscoveryFailed(request.Context(), &task, "upstream", err)
		}
		meta.cacheSource = "upstream"
		writeUpstreamError(writer, routed.Endpoint, err)
		return
	}
	if response.StatusCode == http.StatusNotModified {
		_ = response.Body.Close()
		if created {
			s.markDiscoveryFailed(request.Context(), &task, "upstream", errors.New("upstream confirmed the client's cached representation"))
		}
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
		if created {
			s.markDiscoveryFailed(request.Context(), &task, "upstream", errors.New("upstream manifest request failed"))
		}
		meta.cacheSource = "upstream"
		writeUpstreamStatus(writer, response.StatusCode, routed.Endpoint)
		return
	}
	data, mediaType, digest, err := readUpstreamManifest(response, routed.Reference, s.cfg.Limits.ManifestBytes)
	if err != nil {
		meta.errorClass = upstreamFailureClass(err)
		if created {
			s.markDiscoveryFailed(request.Context(), &task, "manifest_invalid", err)
		}
		meta.cacheSource = "upstream"
		registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream manifest failed validation")
		return
	}
	if _, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes); err != nil {
		meta.errorClass = upstreamFailureClass(err)
		if created {
			s.markDiscoveryFailed(request.Context(), &task, "manifest_invalid", err)
		}
		meta.cacheSource = "upstream"
		registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream manifest failed validation")
		return
	}
	if err := s.cache.Put(request.Context(), routed.Upstream, routed.RemoteRepo, digest, mediaType, bytes.NewReader(data), int64(len(data))); err != nil {
		meta.errorClass = "storage"
		meta.cacheSource = "storage"
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
		return
	}
	if task.ID != "" && (task.Phase == "discovery" || task.RootDigest == "") {
		generation, generationErr := s.generation(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
		if generationErr != nil {
			meta.errorClass = "storage"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "solidification generation state is unavailable")
			return
		}
		if err := s.tasks.SetDiscovered(request.Context(), &task, digest, mediaType, generation.Generation); err != nil {
			latest, getErr := s.tasks.Get(request.Context(), task.ID)
			if getErr != nil || latest.RootDigest != digest {
				meta.errorClass = "storage"
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task state could not be persisted")
				return
			}
		}
	}
	if !acceptsMedia(request.Header.Get("Accept"), mediaType) {
		compatHeaders := requestHeaders(request.Header)
		compatHeaders.Set("Accept", request.Header.Get("Accept"))
		compat, compatErr := s.fetchUpstreamMethod(request.Context(), http.MethodGet, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), nil, compatHeaders)
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
		if readErr != nil || !acceptsMedia(request.Header.Get("Accept"), compatibleType) {
			meta.errorClass = "manifest_invalid"
			meta.cacheSource = "upstream"
			registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no compatible manifest representation is available")
			return
		}
		if _, parseErr := artifact.ParseManifest(body, compatibleType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes); parseErr != nil {
			meta.errorClass = upstreamFailureClass(parseErr)
			meta.cacheSource = "upstream"
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "compatible upstream manifest failed validation")
			return
		}
		if err := s.cache.Put(request.Context(), routed.Upstream, routed.RemoteRepo, compatibleDigest, compatibleType, bytes.NewReader(body), int64(len(body))); err != nil {
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
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.tasks.MarkDiscoveryFailed(persistCtx, task, class, cause)
}

func (s *Service) serveUpstreamBlob(writer http.ResponseWriter, request *http.Request, routed route.Route, meta *requestMeta) {
	cached, err := s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err == nil {
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
	meta.cacheSource = "upstream"
	s.serveCached(writer, request, routed, cached, true)
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
