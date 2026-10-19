package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/route"
	"registry-hub/internal/upstream"
	"strconv"
	"sync"
	"time"
)

func (s *Service) fetchUpstreamMethod(ctx context.Context, method, authority, repository, path string, query url.Values, headers http.Header) (*http.Response, error) {
	waited := time.Duration(0)
	for attempt := 0; attempt < maxUpstreamRetryAttempts; attempt++ {
		release, ok := s.acquireUpstreamRequest(ctx, authority)
		if !ok {
			return nil, errors.New("upstream concurrency limit exceeded")
		}
		var retryAfter string
		s.metrics.recordEvent("upstream_fetch")
		response, err := s.upstream.Do(ctx, authority, repository, method, path, query, headers)
		if err != nil {
			release()
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
			response.Body = &upstreamSlotBody{ReadCloser: response.Body, release: release}
			return response, nil
		} else {
			s.metrics.upstream.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()
			s.metrics.recordEvent("upstream_retry")
			retryAfter = response.Header.Get("Retry-After")
			_ = response.Body.Close()
			release()
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

func (s *Service) fetchUpstreamOnce(ctx context.Context, method, authority, repository, path string, query url.Values, headers http.Header) (*http.Response, error) {
	release, ok := s.acquireUpstreamRequest(ctx, authority)
	if !ok {
		return nil, errors.New("upstream concurrency limit exceeded")
	}
	s.metrics.recordEvent("upstream_fetch")
	response, err := s.upstream.Do(ctx, authority, repository, method, path, query, headers)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		release()
		if status, ok := upstream.HTTPStatus(err); ok {
			s.metrics.upstream.WithLabelValues(strconv.Itoa(status)).Inc()
		} else {
			s.metrics.upstream.WithLabelValues("network_error").Inc()
		}
		return nil, err
	}
	if response == nil || response.Body == nil {
		release()
		return nil, errors.New("upstream returned an empty response")
	}
	s.metrics.upstream.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()
	if response.StatusCode == http.StatusNotFound {
		s.metrics.recordEvent("upstream_404")
	}
	response.Body = &upstreamSlotBody{ReadCloser: response.Body, release: release}
	return response, nil
}

func (s *Service) downloadRetryWaiter() func(context.Context, string, int) error {
	waited := time.Duration(0)
	return func(ctx context.Context, retryAfter string, attempt int) error {
		delay, ok := upstreamRetryDelay(retryAfter, attempt, waited)
		if !ok {
			return errors.New("upstream retry wait budget exhausted")
		}
		if _, hasRetryAfter := parseRetryAfter(retryAfter); !hasRetryAfter {
			waited += delay
		}
		s.metrics.recordEvent("upstream_retry")
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}

func (s *Service) acquireUpstreamRequest(ctx context.Context, authority string) (func(), bool) {
	globalRelease, ok := s.acquireSlot(ctx, s.upstreamSlots)
	if !ok {
		s.metrics.recordRejection("upstream_concurrency")
		return nil, false
	}
	authorityRelease, ok := s.acquireSlot(ctx, s.upstreamSlot(authority))
	if !ok {
		globalRelease()
		s.metrics.recordRejection("upstream_authority_concurrency")
		return nil, false
	}
	return func() {
		authorityRelease()
		globalRelease()
	}, true
}

type upstreamSlotBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *upstreamSlotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
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
