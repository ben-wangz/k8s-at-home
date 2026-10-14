package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"sort"
	"strconv"
	"strings"
)

func (s *Service) writeRegistryResponse(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response, upstreamResponse bool) {
	defer response.Body.Close()
	if !upstreamResponse {
		s.addUnsatisfiedBlobContentRange(request, routed, response)
	}
	if !upstreamResponse && response.StatusCode >= 200 && response.StatusCode < 300 && request.Method != http.MethodHead && (routed.Endpoint == route.EndpointTags || routed.Endpoint == route.EndpointCatalog) {
		s.writeLocalListResponse(writer, request, routed, response)
		return
	}
	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if location := response.Header.Get("Location"); location != "" {
		if upstreamResponse {
			writer.Header().Del("Location")
		} else if routed.Endpoint == route.EndpointUpload && isUploadSessionLocation(location, routed.Repository) {
			writer.Header().Set("Location", location)
		} else if routed.Repository != "" {
			rewritten, err := s.registry.RewriteLocation(location, routed.Internal, routed.Repository)
			if err != nil {
				clearRegistryErrorHeaders(writer.Header())
				registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Location could not be rewritten")
				s.metrics.recordDistributionValidation("location")
				return
			}
			writer.Header().Set("Location", rewritten)
		}
	}
	if link := response.Header.Get("Link"); link != "" && routed.Repository != "" {
		writer.Header().Set("Link", rewritePageLink(link, routed.Repository, ""))
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		clearRegistryErrorHeaders(writer.Header())
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry authentication failed")
		return
	}
	if response.StatusCode >= 400 {
		writeResponseError(writer, response, routed.Endpoint, false)
		return
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, response.Body)
	}
}

func (s *Service) addUnsatisfiedBlobContentRange(request *http.Request, routed route.Route, response *http.Response) {
	if request.Method != http.MethodGet || request.Header.Get("Range") == "" || routed.Endpoint != route.EndpointBlob || response.StatusCode != http.StatusRequestedRangeNotSatisfiable || response.Header.Get("Content-Range") != "" || s.registry == nil {
		return
	}
	head, err := s.registry.Do(request.Context(), http.MethodHead, registryPath(routed), nil, make(http.Header), nil, 0)
	if err != nil {
		return
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusOK || head.ContentLength < 0 {
		return
	}
	response.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", head.ContentLength))
}

func isUploadSessionLocation(location, repository string) bool {
	parsed, err := url.Parse(location)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	path, err := route.ParseURLPath(parsed.EscapedPath())
	if err != nil {
		return false
	}
	prefix := "/v2/" + repository + "/blobs/uploads/"
	identifier := strings.TrimPrefix(path, prefix)
	if !strings.HasPrefix(path, prefix) || identifier == "" || strings.Contains(identifier, "/") {
		return false
	}
	query := parsed.Query()
	return len(query) == 1 && len(query["_state"]) == 1 && query.Get("_state") != ""
}

func (s *Service) writeLocalListResponse(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response) {
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		s.metrics.recordDistributionValidation("list")
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry list response is invalid")
		return
	}
	if routed.Endpoint == route.EndpointTags {
		data, err = normalizeTagList(data, routed.Repository, request.URL.Query(), s.cfg.Limits.MaxPageSize, response.Header, false)
	} else {
		var result struct {
			Repositories []string `json:"repositories"`
		}
		if json.Unmarshal(data, &result) != nil {
			s.metrics.recordDistributionValidation("list")
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal catalog response is invalid")
			return
		}
		repositories := result.Repositories[:0]
		for _, repository := range result.Repositories {
			logical, encoded := route.DecodeInternalName(repository)
			if strings.HasPrefix(repository, "e") && strings.Contains(repository, "/") && !encoded {
				continue
			}
			repositories = append(repositories, logical)
		}
		sort.Strings(repositories)
		result.Repositories = repositories
		data, err = json.Marshal(result)
	}
	if err != nil {
		s.metrics.recordDistributionValidation("list")
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry list response could not be mapped")
		return
	}
	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Del("ETag")
	writer.Header().Del("Docker-Content-Digest")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if link := response.Header.Get("Link"); link != "" {
		endpoint := ""
		if routed.Endpoint == route.EndpointTags {
			endpoint = "tags/list"
		} else {
			endpoint = "_catalog"
		}
		writer.Header().Set("Link", rewritePageLink(link, routed.Repository, endpoint))
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(data)
}

func normalizeTagList(data []byte, repository string, query url.Values, maxPageSize int, headers http.Header, trustSourceLink bool) ([]byte, error) {
	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Tags == nil {
		result.Tags = []string{}
	}
	sort.Strings(result.Tags)
	if last := query.Get("last"); last != "" {
		filtered := result.Tags[:0]
		for _, tag := range result.Tags {
			if tag > last {
				filtered = append(filtered, tag)
			}
		}
		result.Tags = filtered
	}
	result.Name = repository
	sourceLink := headers.Get("Link")
	headers.Del("Link")
	pageSize := maxPageSize
	_, explicitPageSize := query["n"]
	if explicitPageSize {
		pageSize, _ = strconv.Atoi(query.Get("n"))
	}
	more := false
	if pageSize == 0 {
		result.Tags = []string{}
	} else if len(result.Tags) > pageSize {
		more = true
		result.Tags = result.Tags[:pageSize]
	}
	if more {
		values := url.Values{"last": {result.Tags[len(result.Tags)-1]}}
		if explicitPageSize || query.Get("last") != "" || pageSize != maxPageSize {
			values.Set("n", strconv.Itoa(pageSize))
		}
		if query.Get("_origin") == "upstream" {
			values.Set("_origin", "upstream")
		}
		path := "/v2/" + repository + "/tags/list?" + values.Encode()
		headers.Set("Link", "<"+path+`>; rel="next"`)
	} else if sourceLink != "" && pageSize != 0 && trustSourceLink {
		headers.Set("Link", rewritePageLink(sourceLink, repository, "tags/list"))
	}
	return json.Marshal(result)
}

func (s *Service) writeUpstreamResponse(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response) {
	defer response.Body.Close()
	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.Header().Del("WWW-Authenticate")
	writer.Header().Del("Location")
	if response.StatusCode >= 400 {
		writeResponseError(writer, response, routed.Endpoint, true)
		return
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, response.Body)
	}
}

func (s *Service) writeBufferedResponse(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response, data []byte) {
	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	// The body has been normalized, so validators from the upstream response no longer apply.
	writer.Header().Del("ETag")
	writer.Header().Del("Docker-Content-Digest")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if link := response.Header.Get("Link"); link != "" {
		writer.Header().Set("Link", rewritePageLink(link, routed.Repository, "tags/list"))
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(data)
	}
}

func (s *Service) fetchUpstream(ctx context.Context, authority, repository, path string, query url.Values, headers http.Header) (*http.Response, error) {
	return s.fetchUpstreamMethod(ctx, http.MethodGet, authority, repository, path, query, headers)
}
