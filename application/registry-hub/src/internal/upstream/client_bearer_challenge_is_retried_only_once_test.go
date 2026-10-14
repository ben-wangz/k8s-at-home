package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"registry-hub/internal/config"
	"strings"
	"testing"
)

func TestBearerChallengeIsRetriedOnlyOnce(t *testing.T) {
	registryRequests := 0
	tokenRequests := 0
	observed := map[string]int{}
	client := &Client{
		auth: map[string]config.UpstreamAuth{
			"8.8.8.8": {Type: "basic", Username: "registry-user", Password: "registry-password"},
		},
		rules: map[string]config.TransportRule{},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}
			if request.URL.Path == "/token" {
				tokenRequests++
				response.StatusCode = http.StatusOK
				response.Body = io.NopCloser(strings.NewReader(`{"token":"access-token"}`))
				return response, nil
			}
			registryRequests++
			response.StatusCode = http.StatusUnauthorized
			response.Header.Set("WWW-Authenticate", `Bearer realm="https://8.8.8.8/token",service="registry"`)
			return response, nil
		})},
	}
	client.SetObserver(func(event string) { observed[event]++ })
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, nil)
	if err != nil {
		t.Fatalf("upstream request returned error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if registryRequests != 2 || tokenRequests != 1 {
		t.Fatalf("registry requests=%d token requests=%d, want 2 and 1", registryRequests, tokenRequests)
	}
	if observed["upstream_auth_challenge"] != 1 {
		t.Fatalf("Bearer challenge observations = %d, want 1", observed["upstream_auth_challenge"])
	}
}

func TestTokenEndpointStatusRetainsRetryPolicyAndRetryAfter(t *testing.T) {
	client, err := New(config.Config{UpstreamAuth: map[string]config.UpstreamAuth{
		"8.8.8.8": {Type: "basic", Username: "registry-user", Password: "registry-password"},
	}})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}
		if request.URL.Path == "/token" {
			response.StatusCode = http.StatusServiceUnavailable
			response.Header.Set("Retry-After", "7")
			return response, nil
		}
		response.StatusCode = http.StatusUnauthorized
		response.Header.Set("WWW-Authenticate", `Bearer realm="https://8.8.8.8/token",service="registry"`)
		return response, nil
	})

	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, nil)
	if response != nil {
		_ = response.Body.Close()
	}
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusServiceUnavailable || RetryAfter(err) != "7" {
		t.Fatalf("token error = %#v, want 503 with Retry-After 7", err)
	}
	if status, ok := HTTPStatus(err); !ok || status != http.StatusServiceUnavailable {
		t.Fatalf("token HTTP status = %d, %v; want 503, true", status, ok)
	}
	if !Retryable(err) {
		t.Fatal("503 token response is not marked retryable")
	}
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !RetryableStatus(status) {
			t.Errorf("status %d is not retryable", status)
		}
	}
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusBadRequest} {
		if RetryableStatus(status) {
			t.Errorf("status %d is unexpectedly retryable", status)
		}
	}
}

func TestNoProxyMatchHonorsAuthorityPort(t *testing.T) {
	if !noProxyMatch("registry.example", "443", "registry.example:443") {
		t.Fatal("expected host and matching port to bypass the proxy")
	}
	if noProxyMatch("registry.example", "8443", "registry.example:443") {
		t.Fatal("unexpected proxy bypass for a different port")
	}
}

func TestCanonicalDialAddressUsesDefaultProxyPortKey(t *testing.T) {
	proxy, err := url.Parse("http://[2001:db8::1]")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{proxiesByHost: make(map[string]struct{})}
	client.markProxy(proxy)
	if _, ok := client.proxiesByHost[canonicalDialAddress(net.JoinHostPort(proxy.Hostname(), "80"))]; !ok {
		t.Fatal("proxy default port was not recorded in canonical form")
	}
}

func TestSelectProxyUsesRedirectTargetAuthority(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	client := &Client{
		proxies: config.ProxyConfig{Authorities: map[string]string{
			"source.example:443": "http://source-proxy:8080",
			"target.example:443": "http://target-proxy:8080",
		}},
		proxiesByHost: make(map[string]struct{}),
	}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "source.example:443")
	ctx = context.WithValue(ctx, transportAuthorityContextKey{}, "source.example:443")
	request := (&http.Request{URL: &url.URL{Scheme: "https", Host: "target.example:443"}}).WithContext(ctx)
	proxy, err := client.selectProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if proxy == nil || proxy.Host != "target-proxy:8080" {
		t.Fatalf("proxy = %v, want target-proxy:8080", proxy)
	}
}

func TestSelectProxyKeepsDockerAliasAuthority(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	client := &Client{
		proxies: config.ProxyConfig{Authorities: map[string]string{
			"docker.io":      "http://docker-proxy:8080",
			"auth.docker.io": "http://auth-proxy:8080",
		}},
		proxiesByHost: make(map[string]struct{}),
	}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "docker.io")
	ctx = context.WithValue(ctx, transportAuthorityContextKey{}, "registry-1.docker.io")
	request := (&http.Request{URL: &url.URL{Scheme: "https", Host: "auth.docker.io"}}).WithContext(ctx)
	proxy, err := client.selectProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if proxy == nil || proxy.Host != "docker-proxy:8080" {
		t.Fatalf("proxy = %v, want docker-proxy:8080", proxy)
	}
}

func TestSelectProxyUsesAuthorityBeforeDefaultAndHonorsNoProxy(t *testing.T) {
	clearProxyEnvironment(t)
	client := &Client{
		proxies: config.ProxyConfig{
			Authorities: map[string]string{"8.8.8.8": "http://authority-proxy:8080"},
			Default:     "http://default-proxy:8080",
		},
		proxiesByHost: make(map[string]struct{}),
	}
	proxyFor := func(authority string) *url.URL {
		t.Helper()
		ctx := context.WithValue(context.Background(), authorityContextKey{}, authority)
		ctx = context.WithValue(ctx, transportAuthorityContextKey{}, authority)
		request := (&http.Request{URL: &url.URL{Scheme: "https", Host: authority}}).WithContext(ctx)
		proxy, err := client.selectProxy(request)
		if err != nil {
			t.Fatal(err)
		}
		return proxy
	}
	if proxy := proxyFor("8.8.8.8"); proxy == nil || proxy.Host != "authority-proxy:8080" {
		t.Fatalf("authority proxy = %v, want authority-proxy:8080", proxy)
	}
	if proxy := proxyFor("1.1.1.1"); proxy == nil || proxy.Host != "default-proxy:8080" {
		t.Fatalf("default proxy = %v, want default-proxy:8080", proxy)
	}
	t.Setenv("NO_PROXY", "8.8.8.8")
	if proxy := proxyFor("8.8.8.8"); proxy != nil {
		t.Fatalf("NO_PROXY match selected proxy %v", proxy)
	}
	client.proxies = config.ProxyConfig{}
	t.Setenv("NO_PROXY", "")
	if proxy := proxyFor("8.8.8.8"); proxy != nil {
		t.Fatalf("unconfigured proxy selection = %v, want direct", proxy)
	}
}
