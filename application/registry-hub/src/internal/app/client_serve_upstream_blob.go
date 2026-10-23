package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

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
		var persistenceErr *download.PersistenceError
		var guardErr *download.GuardError
		if errors.Is(err, context.Canceled) && request.Context().Err() != nil {
			return
		}
		if errors.As(err, &persistenceErr) || errors.As(err, &guardErr) || errors.Is(err, download.ErrQuotaExceeded) || isStorageFailure(err) {
			if errors.Is(err, lease.ErrLost) || errors.Is(err, storage.ErrLeaseLost) {
				meta.errorClass = "fencing"
			} else {
				meta.errorClass = "storage"
			}
			if persistenceErr != nil && s.logger != nil {
				s.logger.Warn("blob download failed", "request_id", meta.requestID, "failure_stage", persistenceErr.Stage, "error_class", meta.errorClass)
			}
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "verified blob is temporarily unavailable")
			return
		}
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
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "upstream blob could not be cached")
		return
	}
	cached, err = s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference)
	if err != nil {
		meta.errorClass = "storage"
		writer.Header().Set("Retry-After", "5")
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
