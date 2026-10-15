package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"strconv"
	"strings"
)

func (s *Service) serveUpstreamHead(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	clientAccept := combinedAccept(request.Header)
	if routed.Endpoint == route.EndpointManifest && !knownAcceptSupported(clientAccept) {
		meta.cacheSource = "upstream"
		meta.errorClass = "manifest_negotiation"
		registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no supported manifest media type was requested")
		return
	}
	if routed.Endpoint == route.EndpointBlob {
		if cached, err := s.cache.GetAny(request.Context(), routed.Upstream, routed.RemoteRepo, routed.Reference); err == nil {
			_ = cached.Body.Close()
			meta.cacheSource = "cache"
			writeHead(writer, cached.MediaType, cached.Digest, cached.Size)
			return
		} else if !errors.Is(err, storage.ErrNotFound) {
			meta.errorClass = "storage"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "temporary storage is unavailable")
			return
		}
	}
	response, err := s.fetchUpstreamMethod(request.Context(), request.Method, routed.Upstream, routed.RemoteRepo, upstreamPath(routed), query, requestHeaders(request.Header))
	if err != nil {
		writeUpstreamError(writer, routed.Endpoint, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK && (routed.Endpoint == route.EndpointBlob || routed.Endpoint == route.EndpointManifest) && (response.Header.Get("Docker-Content-Digest") == "" || response.ContentLength < 0) {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream HEAD omitted required Registry metadata")
		return
	}
	if response.StatusCode == http.StatusOK && routed.Endpoint == route.EndpointManifest {
		mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || mediaType == "" {
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream HEAD omitted a valid manifest Content-Type")
			return
		}
		if !acceptsMedia(clientAccept, mediaType) {
			meta.errorClass = "manifest_negotiation"
			registryapi.WriteError(writer, http.StatusNotAcceptable, "MANIFEST_INVALID", "no compatible manifest representation is available")
			return
		}
		if !route.ValidDigest(response.Header.Get("Docker-Content-Digest")) {
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream HEAD returned an invalid manifest digest")
			return
		}
		if strings.Contains(routed.Reference, ":") && response.Header.Get("Docker-Content-Digest") != routed.Reference {
			registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream HEAD digest does not match the request")
			return
		}
	}
	if response.StatusCode == http.StatusOK && routed.Endpoint == route.EndpointBlob {
		if !route.ValidDigest(response.Header.Get("Docker-Content-Digest")) || response.Header.Get("Docker-Content-Digest") != routed.Reference {
			registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream HEAD digest does not match the request")
			return
		}
	}
	if response.StatusCode == http.StatusOK && routed.Endpoint == route.EndpointManifest && strings.Contains(routed.Reference, ":") && response.Header.Get("Docker-Content-Digest") != routed.Reference {
		registryapi.WriteError(writer, http.StatusBadGateway, "MANIFEST_INVALID", "upstream HEAD digest does not match the request")
		return
	}
	meta.cacheSource = "upstream"
	s.writeUpstreamResponse(writer, request, routed, response)
}

func (s *Service) serveUpstreamTags(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	if query.Get("n") == "0" {
		headers := make(http.Header)
		data, err := normalizeTagList([]byte(`{"tags":[]}`), routed.Repository, query, s.cfg.Limits.MaxPageSize, headers, false)
		if err != nil {
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream tags response is invalid")
			return
		}
		meta.cacheSource = "upstream"
		s.writeBufferedResponse(writer, request, routed, &http.Response{StatusCode: http.StatusOK, Header: headers}, data)
		return
	}
	upstreamPageSize := s.cfg.Limits.MaxPageSize
	if upstreamPageSize < int(^uint(0)>>1) {
		upstreamPageSize++
	}
	upstreamQuery := url.Values{"n": {strconv.Itoa(upstreamPageSize)}}
	response, err := s.fetchUpstream(request.Context(), routed.Upstream, routed.RemoteRepo, upstreamPath(routed), upstreamQuery, requestHeaders(request.Header))
	if err != nil {
		writeUpstreamError(writer, routed.Endpoint, err)
		return
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		writeUpstreamStatus(writer, response.StatusCode, routed.Endpoint)
		return
	}
	data, err := collectUpstreamTagPages(response, upstreamPath(routed), routed.Upstream, s.cfg.Limits.MaxReferrerPages, upstreamPageSize, 4<<20, func(next url.Values) (*http.Response, error) {
		return s.fetchUpstream(request.Context(), routed.Upstream, routed.RemoteRepo, upstreamPath(routed), next, requestHeaders(request.Header))
	})
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream tags response is invalid")
		return
	}
	headers := make(http.Header)
	paginationQuery := cloneValues(query)
	paginationQuery.Set("_origin", "upstream")
	data, err = normalizeTagList(data, routed.Repository, paginationQuery, s.cfg.Limits.MaxPageSize, headers, false)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream tags response is invalid")
		return
	}
	meta.cacheSource = "upstream"
	s.writeBufferedResponse(writer, request, routed, &http.Response{StatusCode: http.StatusOK, Header: headers}, data)
}

func collectUpstreamTagPages(first *http.Response, expectedPath, authority string, maxPages, maxPageSize, maxBytes int, fetch func(url.Values) (*http.Response, error)) ([]byte, error) {
	tags := make(map[string]struct{})
	seenPages := make(map[string]struct{})
	totalBytes := 0
	response := first
	for page := 0; page < maxPages; page++ {
		if response == nil || response.StatusCode != http.StatusOK {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return nil, errors.New("upstream tags page returned a non-200 status")
		}
		links := response.Header.Values("Link")
		remaining := maxBytes - totalBytes
		data, err := io.ReadAll(io.LimitReader(response.Body, int64(remaining)+1))
		_ = response.Body.Close()
		if err != nil || len(data) > remaining {
			return nil, errors.New("upstream tags response exceeds the size limit")
		}
		totalBytes += len(data)
		var document struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, errors.New("upstream tags response is invalid")
		}
		for _, tag := range document.Tags {
			if !route.ValidReference(tag) || strings.Contains(tag, ":") {
				return nil, errors.New("upstream tags response contains an invalid tag")
			}
			tags[tag] = struct{}{}
		}
		next, found, err := nextTagPage(links, expectedPath, authority, maxPageSize)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		pageKey := next.Encode()
		if _, duplicate := seenPages[pageKey]; duplicate {
			return nil, errors.New("upstream tags pagination loop detected")
		}
		seenPages[pageKey] = struct{}{}
		if page == maxPages-1 {
			return nil, errors.New("upstream tags page limit exceeded")
		}
		response, err = fetch(next)
		if err != nil {
			return nil, err
		}
	}
	ordered := make([]string, 0, len(tags))
	for tag := range tags {
		ordered = append(ordered, tag)
	}
	sort.Strings(ordered)
	return json.Marshal(struct {
		Tags []string `json:"tags"`
	}{Tags: ordered})
}

func nextTagPage(headers []string, expectedPath, authority string, maxPageSize int) (url.Values, bool, error) {
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			start, end := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
			if start < 0 || end <= start || !strings.Contains(strings.ToLower(part[end+1:]), `rel="next"`) && !strings.Contains(strings.ToLower(part[end+1:]), "rel=next") {
				continue
			}
			target, err := url.Parse(strings.TrimSpace(part[start+1 : end]))
			if err != nil || target.User != nil || target.Fragment != "" || target.Opaque != "" || target.Path != "" && target.Path != expectedPath {
				return nil, false, errors.New("upstream tags Link target is invalid")
			}
			if target.Host != "" && !sameUpstreamLinkHost(authority, target.Host) {
				return nil, false, errors.New("upstream tags Link host is invalid")
			}
			if target.Scheme != "" && target.Scheme != "http" && target.Scheme != "https" {
				return nil, false, errors.New("upstream tags Link scheme is invalid")
			}
			query, err := url.ParseQuery(target.RawQuery)
			if err != nil || len(query) == 0 {
				return nil, false, errors.New("upstream tags Link query is invalid")
			}
			for key, values := range query {
				if key != "last" && key != "n" || len(values) != 1 {
					return nil, false, errors.New("upstream tags Link query is invalid")
				}
			}
			if last := query.Get("last"); last == "" || !route.ValidReference(last) || strings.Contains(last, ":") {
				return nil, false, errors.New("upstream tags Link cursor is invalid")
			}
			if rawPageSize := query.Get("n"); rawPageSize != "" {
				pageSize, parseErr := strconv.Atoi(rawPageSize)
				if parseErr != nil || pageSize < 1 || pageSize > maxPageSize {
					return nil, false, errors.New("upstream tags Link page size is invalid")
				}
			}
			return query, true, nil
		}
	}
	return nil, false, nil
}

func sameUpstreamLinkHost(authority, host string) bool {
	if strings.EqualFold(authority, host) {
		return true
	}
	if strings.EqualFold(authority, "docker.io") && strings.EqualFold(host, "registry-1.docker.io") {
		return true
	}
	if strings.HasSuffix(strings.ToLower(host), ":443") {
		return strings.EqualFold(authority, strings.TrimSuffix(host, ":443"))
	}
	return false
}
