package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/upstream"
	"strings"
	"testing"
	"time"
)

type upstreamResult struct {
	status           int
	retryAfter       string
	contentType      string
	body             string
	headers          http.Header
	contentLength    int64
	contentLengthSet bool
	err              error
}

type upstreamCall struct {
	method          string
	path            string
	accept          string
	rangeHeader     string
	ifNoneMatch     string
	ifModifiedSince string
}

type scriptedUpstream struct {
	results []upstreamResult
	calls   []upstreamCall
}

func (u *scriptedUpstream) Do(_ context.Context, _, _, method, path string, _ url.Values, headers http.Header) (*http.Response, error) {
	u.calls = append(u.calls, upstreamCall{
		method: method, path: path, accept: headers.Get("Accept"), rangeHeader: headers.Get("Range"),
		ifNoneMatch: headers.Get("If-None-Match"), ifModifiedSince: headers.Get("If-Modified-Since"),
	})
	result := u.results[len(u.calls)-1]
	if result.err != nil {
		return nil, result.err
	}
	header := make(http.Header)
	if result.contentType != "" {
		header.Set("Content-Type", result.contentType)
	}
	if result.retryAfter != "" {
		header.Set("Retry-After", result.retryAfter)
	}
	for name, values := range result.headers {
		header[name] = append([]string(nil), values...)
	}
	body := result.body
	if body == "" {
		body = "response"
	}
	contentLength := int64(len(body))
	if result.contentLengthSet {
		contentLength = result.contentLength
	}
	return &http.Response{
		StatusCode:    result.status,
		Header:        header,
		ContentLength: contentLength,
		Body:          io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (*scriptedUpstream) Close() {}

func retryTestService(remote upstreamRequester) *Service {
	service := &Service{
		upstream:      remote,
		metrics:       newMetrics(),
		upstreamSlots: make(chan struct{}, 32),
	}
	for index := range service.upstreamPerKey {
		service.upstreamPerKey[index] = make(chan struct{}, 8)
	}
	return service
}

func TestFetchUpstreamRetriesOnlyTransientStatusesAndNetworkErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		first      upstreamResult
		wantCalls  int
		wantStatus int
	}{
		{name: "408", first: upstreamResult{status: http.StatusRequestTimeout, retryAfter: "0"}, wantCalls: 2, wantStatus: http.StatusOK},
		{name: "429", first: upstreamResult{status: http.StatusTooManyRequests, retryAfter: "0"}, wantCalls: 2, wantStatus: http.StatusOK},
		{name: "502", first: upstreamResult{status: http.StatusBadGateway, retryAfter: "0"}, wantCalls: 2, wantStatus: http.StatusOK},
		{name: "503", first: upstreamResult{status: http.StatusServiceUnavailable, retryAfter: "0"}, wantCalls: 2, wantStatus: http.StatusOK},
		{name: "504", first: upstreamResult{status: http.StatusGatewayTimeout, retryAfter: "0"}, wantCalls: 2, wantStatus: http.StatusOK},
		{name: "other 5xx is terminal", first: upstreamResult{status: http.StatusInternalServerError}, wantCalls: 1, wantStatus: http.StatusInternalServerError},
		{name: "404 is terminal", first: upstreamResult{status: http.StatusNotFound}, wantCalls: 1, wantStatus: http.StatusNotFound},
		{name: "auth failure is terminal", first: upstreamResult{status: http.StatusUnauthorized}, wantCalls: 1, wantStatus: http.StatusUnauthorized},
		{name: "temporary network error", first: upstreamResult{err: &net.DNSError{IsTimeout: true}}, wantCalls: 2, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{test.first, {status: http.StatusOK}}}
			response, err := retryTestService(remote).fetchUpstreamMethod(context.Background(), http.MethodGet,
				"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
			if err != nil {
				t.Fatalf("fetchUpstreamMethod returned error: %v", err)
			}
			defer response.Body.Close()
			if len(remote.calls) != test.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", len(remote.calls), test.wantCalls)
			}
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
		})
	}
}

func TestFetchUpstreamPreservesGETAndHEADAcrossRetries(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		remote := &scriptedUpstream{results: []upstreamResult{
			{status: http.StatusServiceUnavailable, retryAfter: "0"},
			{status: http.StatusOK},
		}}
		response, err := retryTestService(remote).fetchUpstreamMethod(context.Background(), method,
			"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
		if err != nil {
			t.Fatalf("%s retry returned error: %v", method, err)
		}
		_ = response.Body.Close()
		if len(remote.calls) != 2 || remote.calls[0].method != method || remote.calls[1].method != method {
			t.Fatalf("%s retry calls = %#v", method, remote.calls)
		}
	}
}

func TestFetchUpstreamStopsAfterThreeAttempts(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
	}}
	response, err := retryTestService(remote).fetchUpstreamMethod(context.Background(), http.MethodGet,
		"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
	if response != nil || err == nil {
		t.Fatalf("exhausted retry returned response=%v error=%v", response, err)
	}
	if len(remote.calls) != maxUpstreamRetryAttempts {
		t.Fatalf("upstream calls = %d, want %d", len(remote.calls), maxUpstreamRetryAttempts)
	}
}

type retryStatusErrorUpstream struct {
	status       int
	retryAfter   string
	failuresLeft int
	calls        int
}

func (remote *retryStatusErrorUpstream) Do(context.Context, string, string, string, string, url.Values, http.Header) (*http.Response, error) {
	remote.calls++
	if remote.failuresLeft > 0 {
		remote.failuresLeft--
		return nil, &upstream.HTTPStatusError{StatusCode: remote.status, RetryAfter: remote.retryAfter}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func (*retryStatusErrorUpstream) Close() {}

func TestFetchUpstreamRetriesTokenStatusErrorsWithinSharedBudget(t *testing.T) {
	remote := &retryStatusErrorUpstream{status: http.StatusServiceUnavailable, retryAfter: "0", failuresLeft: 2}
	service := retryTestService(remote)
	response, err := service.fetchUpstreamMethod(context.Background(), http.MethodGet,
		"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
	if err != nil {
		t.Fatalf("fetch after retryable token status failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || remote.calls != maxUpstreamRetryAttempts {
		t.Fatalf("status=%d upstream attempts=%d, want 200 and %d attempts", response.StatusCode, remote.calls, maxUpstreamRetryAttempts)
	}
	metricsResponse := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := metricsResponse.Body.String(); strings.Count(body, `registry_hub_upstream_requests_total{result="503"} 2`) != 1 || strings.Contains(body, `registry_hub_upstream_requests_total{result="network_error"}`) {
		t.Fatalf("token status metrics do not preserve HTTP classification: %s", body)
	}

	terminal := &retryStatusErrorUpstream{status: http.StatusInternalServerError, retryAfter: "0", failuresLeft: 2}
	response, err = retryTestService(terminal).fetchUpstreamMethod(context.Background(), http.MethodGet,
		"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
	if response != nil || err == nil || terminal.calls != 1 {
		t.Fatalf("terminal token status result response=%v error=%v attempts=%d, want one non-retried attempt", response, err, terminal.calls)
	}
}

func TestFetchUpstreamRetryWaitHonorsRequestDeadline(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusServiceUnavailable, retryAfter: "60"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	response, err := retryTestService(remote).fetchUpstreamMethod(ctx, http.MethodGet,
		"registry.example", "team/image", "/v2/team/image/manifests/latest", nil, nil)
	if response != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline result response=%v error=%v", response, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline cancellation took %s", elapsed)
	}
	if len(remote.calls) != 1 {
		t.Fatalf("upstream calls after deadline = %d, want 1", len(remote.calls))
	}
}
