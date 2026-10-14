package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/route"
	"registry-hub/internal/upstream"
	"strconv"
	"time"
)

func (s *Service) fetchUpstreamMethod(ctx context.Context, method, authority, repository, path string, query url.Values, headers http.Header) (*http.Response, error) {
	globalRelease, ok := s.acquireSlot(ctx, s.upstreamSlots)
	if !ok {
		s.metrics.recordRejection("upstream_concurrency")
		return nil, errors.New("upstream concurrency limit exceeded")
	}
	defer globalRelease()
	perAuthorityRelease, ok := s.acquireSlot(ctx, s.upstreamSlot(authority))
	if !ok {
		s.metrics.recordRejection("upstream_authority_concurrency")
		return nil, errors.New("upstream authority concurrency limit exceeded")
	}
	defer perAuthorityRelease()
	waited := time.Duration(0)
	for attempt := 0; attempt < maxUpstreamRetryAttempts; attempt++ {
		var retryAfter string
		s.metrics.recordEvent("upstream_fetch")
		response, err := s.upstream.Do(ctx, authority, repository, method, path, query, headers)
		if err != nil {
			retryAfter = upstream.RetryAfter(err)
			if status, ok := upstream.HTTPStatus(err); ok {
				s.metrics.upstream.WithLabelValues(strconv.Itoa(status)).Inc()
			} else {
				s.metrics.upstream.WithLabelValues("network_error").Inc()
			}
			if attempt > 0 {
				s.metrics.recordEvent("upstream_retry")
			}
			if !upstream.Retryable(err) || attempt == maxUpstreamRetryAttempts-1 {
				return nil, err
			}
		} else if !retryableStatus(response.StatusCode) {
			s.metrics.upstream.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()
			if response.StatusCode == http.StatusNotFound {
				s.metrics.recordEvent("upstream_404")
			}
			return response, nil
		} else {
			s.metrics.upstream.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()
			s.metrics.recordEvent("upstream_retry")
			retryAfter = response.Header.Get("Retry-After")
			_ = response.Body.Close()
			if attempt == maxUpstreamRetryAttempts-1 {
				return nil, fmt.Errorf("upstream returned status %d", response.StatusCode)
			}
		}
		delay, ok := upstreamRetryDelay(retryAfter, attempt, waited)
		if !ok {
			return nil, errors.New("upstream retry wait budget exhausted")
		}
		if _, hasRetryAfter := parseRetryAfter(retryAfter); !hasRetryAfter {
			waited += delay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("upstream retry budget exhausted")
}

func registryPath(routed route.Route) string {
	if routed.Endpoint == route.EndpointCatalog {
		return "/v2/_catalog"
	}
	if routed.Endpoint == route.EndpointV2 {
		return "/v2/"
	}
	base := "/v2/" + routed.Internal
	switch routed.Endpoint {
	case route.EndpointManifest:
		return base + "/manifests/" + url.PathEscape(routed.Reference)
	case route.EndpointBlob:
		return base + "/blobs/" + url.PathEscape(routed.Reference)
	case route.EndpointUpload:
		if routed.UploadID == "" {
			return base + "/blobs/uploads/"
		}
		return base + "/blobs/uploads/" + url.PathEscape(routed.UploadID)
	case route.EndpointTags:
		return base + "/tags/list"
	case route.EndpointReferrers:
		return base + "/referrers/" + url.PathEscape(routed.Reference)
	default:
		return base
	}
}

func upstreamPath(routed route.Route) string {
	base := "/v2/" + routed.RemoteRepo
	switch routed.Endpoint {
	case route.EndpointManifest:
		return base + "/manifests/" + url.PathEscape(routed.Reference)
	case route.EndpointBlob:
		return base + "/blobs/" + url.PathEscape(routed.Reference)
	case route.EndpointTags:
		return base + "/tags/list"
	case route.EndpointReferrers:
		return base + "/referrers/" + url.PathEscape(routed.Reference)
	default:
		return base
	}
}

func requestHeaders(source http.Header) http.Header {
	result := make(http.Header)
	for _, name := range []string{
		"Accept", "Content-Type", "Content-Range", "Range", "If-Range", "If-None-Match", "If-Modified-Since",
		"Docker-Content-Digest", "Docker-Upload-UUID", "OCI-Chunk-Min-Length", "OCI-Subject", "OCI-Tag",
	} {
		for _, value := range source.Values(name) {
			result.Add(name, value)
		}
	}
	result.Set("Accept-Encoding", "identity")
	return result
}

func permissionFor(method string) (string, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return auth.Pull, true
	case http.MethodDelete:
		return auth.Delete, true
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return auth.Push, true
	default:
		return "", false
	}
}

func isWriteMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut || method == http.MethodDelete
}

func readCanFallback(endpoint route.Endpoint) bool {
	return endpoint == route.EndpointManifest || endpoint == route.EndpointBlob || endpoint == route.EndpointTags || endpoint == route.EndpointReferrers
}

func parseQuery(request *http.Request) (url.Values, error) {
	return url.ParseQuery(request.URL.RawQuery)
}
