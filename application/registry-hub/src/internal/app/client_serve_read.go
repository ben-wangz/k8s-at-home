package app

import (
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strconv"
)

func (s *Service) serveRead(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	if routed.Endpoint == route.EndpointReferrers && request.Method == http.MethodHead {
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "Referrers supports GET only")
		return
	}
	if routed.Endpoint == route.EndpointTags && query.Get("_origin") == "upstream" {
		query = cloneValues(query)
		query.Del("_origin")
		s.serveUpstreamTags(writer, request, routed, query, meta)
		return
	}
	if routed.Endpoint == route.EndpointManifest && request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", manifestAcceptFor(""))
	}
	localPath := registryPath(routed)
	localQuery := cloneValues(query)
	if routed.Endpoint == route.EndpointTags {
		pageSize := s.cfg.Limits.MaxPageSize
		if value := query.Get("n"); value != "" {
			pageSize, _ = strconv.Atoi(value)
		}
		if pageSize > 0 {
			localQuery.Set("n", strconv.Itoa(pageSize+1))
		} else {
			localQuery.Set("n", "0")
		}
	}
	if routed.Endpoint == route.EndpointReferrers {
		localQuery = s.referrerSourceQuery(query)
	}
	if routed.Endpoint == route.EndpointCatalog && localQuery.Get("last") != "" {
		internalLast, mapErr := route.InternalName(localQuery.Get("last"))
		if mapErr != nil {
			registryapi.WriteError(writer, http.StatusBadRequest, "NAME_INVALID", "catalog cursor is invalid")
			return
		}
		localQuery.Set("last", internalLast)
	}
	local, err := s.registry.Do(request.Context(), request.Method, localPath, localQuery, requestHeaders(request.Header), nil, 0)
	if err != nil {
		meta.cacheSource = "error"
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry is unavailable")
		return
	}
	if local.StatusCode == http.StatusUnauthorized || local.StatusCode == http.StatusForbidden {
		_ = local.Body.Close()
		meta.cacheSource = "error"
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry authentication failed")
		return
	}
	if routed.Endpoint == route.EndpointReferrers {
		s.serveLocalReferrers(writer, request, routed, query, local, meta)
		return
	}
	if local.StatusCode != http.StatusNotFound || !readCanFallback(routed.Endpoint) || !confirmedMiss(local, request.Method, routed.Endpoint) {
		meta.cacheSource = "local"
		if routed.Endpoint == route.EndpointManifest {
			s.writeLocalManifestResponse(writer, request, routed, local)
		} else {
			s.writeRegistryResponse(writer, request, routed, local, false)
		}
		return
	}
	if routed.Endpoint == route.EndpointManifest {
		probe := s.localManifestProbe(request, routed, query)
		if probe != nil {
			if probe.StatusCode != http.StatusNotFound {
				meta.cacheSource = "local"
				s.writeLocalManifestResponse(writer, request, routed, probe)
				return
			}
			_ = probe.Body.Close()
		}
	}
	_ = local.Body.Close()
	if !routed.HasUpstream {
		meta.cacheSource = "local"
		writeMiss(writer, routed.Endpoint)
		return
	}
	if request.Method == http.MethodHead {
		s.serveUpstreamHead(writer, request, routed, query, meta)
		return
	}
	switch routed.Endpoint {
	case route.EndpointManifest:
		s.serveUpstreamManifest(writer, request, routed, meta)
	case route.EndpointBlob:
		s.serveUpstreamBlob(writer, request, routed, meta)
	case route.EndpointTags:
		s.serveUpstreamTags(writer, request, routed, query, meta)
	case route.EndpointReferrers:
		s.serveUpstreamReferrers(writer, request, routed, query, meta)
	default:
		writeMiss(writer, routed.Endpoint)
	}
}

func (s *Service) localManifestProbe(request *http.Request, routed route.Route, query url.Values) *http.Response {
	headers := requestHeaders(request.Header)
	headers.Set("Accept", manifestAcceptFor(request.Header.Get("Accept")))
	response, err := s.registry.Do(request.Context(), request.Method, registryPath(routed), query, headers, nil, 0)
	if err != nil {
		return nil
	}
	return response
}

func (s *Service) writeLocalManifestResponse(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response) {
	if response.StatusCode == http.StatusOK {
		mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || mediaType == "" {
			_ = response.Body.Close()
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "internal Registry manifest Content-Type is invalid")
			return
		}
		if !acceptsMedia(request.Header.Get("Accept"), mediaType) {
			_ = response.Body.Close()
			registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no compatible local manifest representation is available")
			return
		}
	}
	s.writeRegistryResponse(writer, request, routed, response, false)
}
