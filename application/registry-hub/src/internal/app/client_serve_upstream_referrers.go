package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
)

func (s *Service) serveUpstreamReferrers(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	response, err := s.fetchUpstream(request.Context(), routed.Upstream, routed.RemoteRepo, upstreamPath(routed), s.referrerSourceQuery(query), requestHeaders(request.Header))
	if err != nil {
		writeUpstreamError(writer, routed.Endpoint, err)
		return
	}
	if response.StatusCode == http.StatusNotFound {
		if !confirmedMiss(response, http.MethodGet, route.EndpointReferrers) {
			meta.cacheSource = "upstream"
			s.writeUpstreamResponse(writer, request, routed, response)
			return
		}
		_ = response.Body.Close()
		fallback := upstreamManifestPath(routed, referrerFallbackTag(routed.Reference))
		response, err = s.fetchUpstream(request.Context(), routed.Upstream, routed.RemoteRepo, fallback, nil, http.Header{"Accept": []string{manifestAccept}})
		if err != nil {
			writeUpstreamError(writer, routed.Endpoint, err)
			return
		}
		if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
			_ = response.Body.Close()
			meta.cacheSource = "upstream"
			s.writeReferrerPage(writer, request, routed, query, referrerIndex{manifests: []json.RawMessage{}}, false)
			return
		}
		if response.StatusCode != http.StatusOK {
			meta.cacheSource = "upstream"
			s.writeUpstreamResponse(writer, request, routed, response)
			return
		}
		index, readErr := s.readReferrerIndex(response)
		if readErr != nil {
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream fallback referrers index is invalid")
			return
		}
		meta.cacheSource = "upstream"
		s.writeReferrerPage(writer, request, routed, query, index, false)
		return
	}
	if response.StatusCode != http.StatusOK {
		writeUpstreamError(writer, routed.Endpoint, fmt.Errorf("upstream referrers returned status %d", response.StatusCode))
		_ = response.Body.Close()
		return
	}
	index, err := s.collectUpstreamReferrers(request.Context(), routed, response, query.Get("artifactType"))
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream referrers response is invalid")
		return
	}
	meta.cacheSource = "upstream"
	s.writeReferrerPage(writer, request, routed, query, index, false)
}

func (s *Service) createUpload(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	authority, repository := routeStorageIdentity(routed)
	internalQuery, err := mapUploadQuery(query)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadRequest, "UNSUPPORTED", "upload query is invalid")
		return
	}
	if request.Method != http.MethodPost {
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "upload creation requires POST")
		return
	}
	if request.ContentLength > s.cfg.Limits.UploadBytes {
		registryapi.WriteError(writer, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "upload request exceeds configured limit")
		return
	}
	var releaseContent func() error
	if digest := query.Get("digest"); digest != "" {
		_, releaseContent, err = acquireContentLease(request.Context(), s.tasks.Leases(), authority, repository, digest, requestID())
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "blob state is unavailable")
				return
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "blob digest is being modified")
			return
		}
		defer releaseContent()
	}
	release, err := s.gate.Enter(request.Context())
	if err != nil {
		if isStorageFailure(err) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "Registry writes are blocked")
		return
	}
	defer release()
	if digest := query.Get("digest"); digest != "" {
		if _, err := s.advanceGeneration(request.Context(), authority, repository, digest, false); err != nil {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "blob generation could not be advanced")
			return
		}
	}
	response, err := s.registry.Do(request.Context(), request.Method, registryPath(routed), internalQuery, requestHeaders(request.Header), http.MaxBytesReader(writer, request.Body, s.cfg.Limits.UploadBytes), request.ContentLength)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry is unavailable")
		return
	}
	if response.StatusCode == http.StatusAccepted {
		external, rewriteErr := s.uploads.Create(request.Context(), routed.Repository, routed.Internal, response.Header.Get("Location"))
		if rewriteErr != nil {
			s.abortInternalUpload(response.Header.Get("Location"), routed.Internal)
			_ = response.Body.Close()
			if isStorageFailure(rewriteErr) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "upload session could not be persisted")
				return
			}
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal upload session could not be created")
			return
		}
		response.Header.Set("Location", external)
		parts := strings.Split(external, "/")
		response.Header.Set("Docker-Upload-UUID", strings.SplitN(parts[len(parts)-1], "?", 2)[0])
	}
	meta.cacheSource = "local"
	s.writeRegistryResponse(writer, request, routed, response, false)
}

func (s *Service) proxyUpload(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	authority, repository := routeStorageIdentity(routed)
	if request.ContentLength > s.cfg.Limits.UploadBytes && request.Method != http.MethodGet && request.Method != http.MethodHead {
		registryapi.WriteError(writer, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "upload request exceeds configured limit")
		return
	}
	limited := *request
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		limited.Body = http.MaxBytesReader(writer, request.Body, s.cfg.Limits.UploadBytes)
	}
	var err error
	var releaseContent func() error
	if request.Method == http.MethodPut && query.Get("digest") != "" {
		_, releaseContent, err = acquireContentLease(request.Context(), s.tasks.Leases(), authority, repository, query.Get("digest"), requestID())
		if err != nil {
			if isStorageFailure(err) {
				meta.errorClass = "storage"
				writer.Header().Set("Retry-After", "5")
				registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "blob state is unavailable")
				return
			}
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "blob digest is being modified")
			return
		}
		defer releaseContent()
	}
	var beforeWrite func() error
	if request.Method == http.MethodPut && query.Get("digest") != "" {
		beforeWrite = func() error {
			_, err := s.advanceGeneration(request.Context(), authority, repository, query.Get("digest"), false)
			return err
		}
	}
	response, err := s.uploads.Proxy(&limited, routed.Repository, routed.UploadID, query, s.gate, beforeWrite)
	if errors.Is(err, storage.ErrNotFound) {
		registryapi.WriteError(writer, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session does not exist")
		return
	}
	if err != nil {
		if errors.Is(err, ErrWritesClosed) {
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "Registry writes are blocked")
			return
		}
		if isStorageFailure(err) {
			meta.errorClass = "storage"
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "upload status could not be confirmed")
			return
		}
		registryapi.WriteError(writer, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "upload session is invalid")
		return
	}
	meta.cacheSource = "local"
	s.writeRegistryResponse(writer, request, routed, response, false)
}

func (s *Service) proxyLocalWrite(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	releasePush, ok := s.acquireSlot(request.Context(), s.pushSlots)
	if !ok {
		s.metrics.recordRejection("push_concurrency")
		writer.Header().Set("Retry-After", "1")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "push concurrency limit exceeded")
		return
	}
	defer releasePush()
	release, err := s.gate.Enter(request.Context())
	if err != nil {
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "Registry writes are blocked")
		return
	}
	response, err := s.registry.Do(request.Context(), request.Method, registryPath(routed), query, requestHeaders(request.Header), request.Body, request.ContentLength)
	release()
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry is unavailable")
		return
	}
	meta.cacheSource = "local"
	s.writeRegistryResponse(writer, request, routed, response, false)
}
