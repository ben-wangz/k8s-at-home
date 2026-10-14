package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/lease"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

type requestMetaKey struct{}

type requestMeta struct {
	operation   string
	cacheSource string
	requestID   string
	errorClass  string
}

type heldTagLease struct {
	tag     string
	ctx     context.Context
	lease   lease.Lease
	release func() error
}

func isStorageFailure(err error) bool {
	return errors.Is(err, storage.ErrUnavailable) ||
		errors.Is(err, storage.ErrTimeout) ||
		errors.Is(err, storage.ErrConditionFailed) ||
		errors.Is(err, storage.ErrPermissionDenied) ||
		errors.Is(err, storage.ErrCorruptObject) ||
		errors.Is(err, storage.ErrUnknownCommit) ||
		errors.Is(err, storage.ErrLeaseLost) ||
		errors.Is(err, storage.ErrInvalidKey)
}

func (s *Service) handleClient(original http.ResponseWriter, request *http.Request) {
	started := time.Now()
	recorder := &statusWriter{ResponseWriter: original}
	writer := http.ResponseWriter(recorder)
	meta := &requestMeta{operation: request.Method, cacheSource: "none", requestID: requestID()}
	defer func() {
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.metrics.recordRequest(meta.operation, request.Method, status, meta.cacheSource, meta.errorClass, time.Since(started))
		logRequest := s.logger.Info
		if meta.errorClass == "storage" {
			logRequest = s.logger.Warn
		}
		logRequest("registry request", "request_id", meta.requestID, "operation", meta.operation,
			"method", request.Method, "status", status, "latency_ms", time.Since(started).Milliseconds(),
			"cache_source", meta.cacheSource, "error_class", meta.errorClass)
	}()
	if request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.URL.Path == "/readyz" {
		if !s.ready.Load() {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		return
	}
	if !s.limiter.Allow(s.sourceAddress(request), s.cfg.ClientAuth.Empty()) {
		meta.errorClass = "rate_limit"
		s.metrics.recordRejection("rate_limit")
		writer.Header().Set("Retry-After", "1")
		registryapi.WriteError(writer, http.StatusTooManyRequests, "TOOMANYREQUESTS", "request rate limit exceeded")
		return
	}
	principal, authenticated := auth.Authenticate(request, s.cfg.ClientAuth, true)
	release, ok := s.acquireClient(request, authenticated && !principal.Trusted)
	if !ok {
		meta.errorClass = "concurrency"
		s.metrics.recordRejection("client_concurrency")
		writer.Header().Set("Retry-After", "1")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "request concurrency limit exceeded")
		return
	}
	defer release()
	if !authenticated {
		meta.errorClass = "authentication"
		auth.Challenge(writer, "registry-hub")
		registryapi.WriteError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.cfg.Limits.RequestTimeout)
	defer cancel()
	request = request.WithContext(context.WithValue(ctx, requestMetaKey{}, meta))
	if request.URL.Path == "/v2" {
		writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		writer.WriteHeader(http.StatusOK)
		return
	}
	routed, err := route.Parse(request.RequestURI)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadRequest, "NAME_INVALID", "registry path is invalid")
		return
	}
	meta.operation = string(routed.Endpoint)
	if routed.Endpoint == route.EndpointV2 {
		writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		writer.WriteHeader(http.StatusOK)
		return
	}
	if (routed.Endpoint == route.EndpointTags || routed.Endpoint == route.EndpointCatalog) && request.Method != http.MethodGet {
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "this endpoint supports GET only")
		return
	}
	permission, methodOK := permissionFor(request.Method)
	if !methodOK {
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "method is not supported for this endpoint")
		return
	}
	if routed.Endpoint == route.EndpointCatalog {
		permission = auth.Catalog
	}
	if !principal.Allows(permission) {
		meta.errorClass = "authorization"
		status := http.StatusForbidden
		code := "DENIED"
		if request.Header.Get("Authorization") == "" {
			status, code = http.StatusUnauthorized, "UNAUTHORIZED"
			auth.Challenge(writer, "registry-hub")
		}
		registryapi.WriteError(writer, status, code, "operation is not permitted")
		return
	}
	query, err := parseQuery(request)
	if err != nil || validateQuery(request.Method, routed, query, s.cfg.Limits.MaxPageSize) != nil {
		registryapi.WriteError(writer, http.StatusBadRequest, "UNSUPPORTED", "query parameters are invalid")
		return
	}
	if routed.Endpoint == route.EndpointCatalog && !s.cfg.CatalogEnabled {
		registryapi.WriteError(writer, http.StatusNotFound, "UNSUPPORTED", "catalog is disabled")
		return
	}
	s.serveRoute(writer, request, routed, query, meta)
}

func (s *Service) serveRoute(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	if routed.Endpoint == route.EndpointUpload {
		if routed.UploadID == "" {
			s.createUpload(writer, request, routed, query, meta)
			return
		}
		s.proxyUpload(writer, request, routed, query, meta)
		return
	}
	if routed.Endpoint == route.EndpointManifest && request.Method == http.MethodPut {
		s.pushManifest(writer, request, routed, query, meta)
		return
	}
	if request.Method == http.MethodDelete {
		s.deleteLocal(writer, request, routed, query, meta)
		return
	}
	if isWriteMethod(request.Method) {
		s.proxyLocalWrite(writer, request, routed, query, meta)
		return
	}
	s.serveRead(writer, request, routed, query, meta)
}
