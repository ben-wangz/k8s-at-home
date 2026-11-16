package distribution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPAttempt struct {
	Context    context.Context
	Method     string
	Operation  string
	StatusCode int
	Err        error
	Duration   time.Duration
}

type observedTransport struct {
	base   http.RoundTripper
	client *Client
}

func (t observedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := t.base.RoundTrip(request)
	attempt := HTTPAttempt{
		Context:   request.Context(),
		Method:    boundedHTTPMethod(request.Method),
		Operation: httpOperation(request.URL.Path),
		Err:       err,
		Duration:  time.Since(started),
	}
	if response != nil {
		attempt.StatusCode = response.StatusCode
	}
	t.client.observeHTTPAttempt(attempt)
	return response, err
}

func boundedHTTPMethod(method string) string {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
		return strings.ToUpper(method)
	default:
		return "OTHER"
	}
}

func httpOperation(requestPath string) string {
	segments := strings.Split(strings.Trim(requestPath, "/"), "/")
	v2Index := -1
	for index, segment := range segments {
		if segment == "v2" {
			v2Index = index
			break
		}
	}
	if v2Index < 0 {
		return "other"
	}
	segments = segments[v2Index+1:]
	if len(segments) == 0 {
		return "api"
	}
	if len(segments) == 1 && segments[0] == "_catalog" {
		return "catalog"
	}
	for _, segment := range segments[1:] {
		switch segment {
		case "manifests", "blobs", "referrers", "uploads":
			return segment
		case "tags":
			return "tags"
		}
	}
	return "other"
}

func HTTPAttemptResult(attempt HTTPAttempt) string {
	if attempt.Err != nil {
		var networkError net.Error
		if errors.Is(attempt.Err, context.DeadlineExceeded) || (errors.As(attempt.Err, &networkError) && networkError.Timeout()) {
			return "timeout"
		}
		return "transport_error"
	}
	switch {
	case attempt.StatusCode == http.StatusNotFound:
		return "not_found"
	case attempt.StatusCode == http.StatusConflict || attempt.StatusCode == http.StatusPreconditionFailed:
		return "condition_failed"
	case attempt.StatusCode >= 500:
		return "server_error"
	case attempt.StatusCode >= 400:
		return "client_error"
	case attempt.StatusCode >= 200 && attempt.StatusCode < 400:
		return "success"
	default:
		return "other"
	}
}

func (c *Client) Do(ctx context.Context, method, registryPath string, query url.Values, headers http.Header, body io.Reader, contentLength int64) (*http.Response, error) {
	request, err := c.request(ctx, method, registryPath, query, headers, body, contentLength)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	c.observeResponse(response, err)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && c.token != "" && sameOriginBearerChallenge(response.Header.Values("WWW-Authenticate"), c.base) {
		var retryBody io.Reader
		if request.GetBody != nil {
			retryBody, err = request.GetBody()
		} else if body == nil {
			retryBody = nil
		} else if seeker, ok := body.(io.Seeker); ok {
			_, err = seeker.Seek(0, io.SeekStart)
			retryBody = body
		} else {
			// Retrying a non-rewindable body would silently send an empty
			// request to Distribution, so preserve the challenge response.
			return response, nil
		}
		if err != nil {
			return response, nil
		}
		_ = response.Body.Close()
		request, err = c.request(ctx, method, registryPath, query, headers, retryBody, contentLength)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+c.token)
		response, err = c.client.Do(request)
		c.observeResponse(response, err)
		return response, err
	}
	return response, nil
}

func (c *Client) Ping(ctx context.Context) error {
	response, err := c.Do(ctx, http.MethodGet, "/v2/", nil, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Distribution /v2/ returned status %d", response.StatusCode)
	}
	return nil
}

func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}
