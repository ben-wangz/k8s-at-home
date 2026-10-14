package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"registry-hub/internal/config"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectProxyEnvironmentOverridesConfiguredProxy(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_PROXY_ENV_TEST") == "1" {
		client := &Client{
			proxies:       config.ProxyConfig{Authorities: map[string]string{"8.8.8.8": "http://authority-proxy:8080"}},
			proxiesByHost: make(map[string]struct{}),
		}
		ctx := context.WithValue(context.Background(), authorityContextKey{}, "8.8.8.8")
		request := (&http.Request{URL: &url.URL{Scheme: "https", Host: "8.8.8.8"}}).WithContext(ctx)
		proxy, err := client.selectProxy(request)
		if err != nil {
			t.Fatal(err)
		}
		if proxy == nil || proxy.Host != "environment-proxy:8080" {
			t.Fatalf("environment proxy = %v, want environment-proxy:8080", proxy)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestSelectProxyEnvironmentOverridesConfiguredProxy$")
	command.Env = []string{
		"REGISTRY_HUB_PROXY_ENV_TEST=1",
		"HTTPS_PROXY=http://environment-proxy:8080",
		"HTTP_PROXY=",
		"https_proxy=",
		"http_proxy=",
		"NO_PROXY=",
		"no_proxy=",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("environment proxy subprocess failed: %v\n%s", err, output)
	}
}

func TestProxyFailureDoesNotFallbackAndCredentialsStaySeparate(t *testing.T) {
	clearProxyEnvironment(t)
	requestHeaders := make(chan http.Header, 1)
	var proxyRequests atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyRequests.Add(1)
		requestHeaders <- request.Header.Clone()
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("proxy-user", "proxy-password")
	client, err := New(config.Config{
		UpstreamAuth: map[string]config.UpstreamAuth{
			"8.8.8.8": {Type: "bearer", Token: "registry-token"},
		},
		TransportRules: map[string]config.TransportRule{
			"8.8.8.8": {AllowHTTP: true},
		},
		Proxy: config.ProxyConfig{Authorities: map[string]string{"8.8.8.8": proxyURL.String()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var directDials atomic.Int32
	client.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "8.8.8.8:") {
			directDials.Add(1)
			return nil, errors.New("direct fallback attempted")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, http.Header{
			"Proxy-Authorization": []string{"Basic caller-proxy-credential"},
			"Cookie":              []string{"client-cookie"},
		})
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("failed proxy connection unexpectedly succeeded")
	}
	if proxyRequests.Load() != 1 || directDials.Load() != 0 {
		t.Fatalf("proxy requests=%d direct dials=%d, want 1 and 0", proxyRequests.Load(), directDials.Load())
	}
	select {
	case headers := <-requestHeaders:
		if got, want := headers.Get("Proxy-Authorization"), "Basic cHJveHktdXNlcjpwcm94eS1wYXNzd29yZA=="; got != want {
			t.Fatalf("proxy credential = %q, want %q", got, want)
		}
		if got := headers.Get("Authorization"); got != "Bearer registry-token" {
			t.Fatalf("Registry credential = %q", got)
		}
		if headers.Get("Cookie") != "" {
			t.Fatalf("client cookie reached proxy request: %q", headers.Get("Cookie"))
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive a request")
	}
}

func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
}

func TestRetryableRejectsTLSValidationErrors(t *testing.T) {
	if Retryable(&url.Error{Op: "Get", URL: "https://registry.example", Err: x509.UnknownAuthorityError{}}) {
		t.Fatal("TLS validation failure was classified as retryable")
	}
}

func TestRetryableAcceptsTransientNetworkErrors(t *testing.T) {
	if !Retryable(&net.DNSError{IsTimeout: true}) {
		t.Fatal("timeout network error was not classified as retryable")
	}
	if !Retryable(io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF was not classified as retryable")
	}
}

func TestRedirectUsesTargetAuthorityTransportRule(t *testing.T) {
	observed := map[string]int{}
	client := &Client{rules: map[string]config.TransportRule{
		"registry.example:5000": {Ports: []uint16{5000}},
		"8.8.8.8:5000":          {Ports: []uint16{5000}},
	}}
	client.SetObserver(func(event string) { observed[event]++ })
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "registry.example:5000")
	request := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "8.8.8.8:5000"},
		Header: make(http.Header),
	}
	request = request.WithContext(ctx)
	previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example:5000"}}}
	if err := client.checkRedirect(request, previous); err != nil {
		t.Fatalf("redirect should use the target authority transport rule: %v", err)
	}
	if observed["upstream_redirect"] != 1 {
		t.Fatalf("redirect observations = %d, want 1", observed["upstream_redirect"])
	}
}

func TestCrossHostRedirectDropsSourceCredentialsAndUsesTargetCredentials(t *testing.T) {
	for _, test := range []struct {
		name       string
		targetAuth config.UpstreamAuth
		wantAuth   string
	}{
		{name: "target credential", targetAuth: config.UpstreamAuth{Type: "basic", Username: "target-user", Password: "target-password"}, wantAuth: "Basic dGFyZ2V0LXVzZXI6dGFyZ2V0LXBhc3N3b3Jk"},
		{name: "anonymous target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := map[string]config.UpstreamAuth{
				"registry.example:5000": {Type: "bearer", Token: "source-token"},
			}
			if test.targetAuth.Type != "" {
				auth["8.8.8.8:5000"] = test.targetAuth
			}
			client := &Client{
				auth: auth,
				rules: map[string]config.TransportRule{
					"8.8.8.8:5000": {Ports: []uint16{5000}},
				},
			}
			ctx := context.WithValue(context.Background(), authorityContextKey{}, "registry.example:5000")
			ctx = context.WithValue(ctx, transportAuthorityContextKey{}, "registry.example:5000")
			request := (&http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Scheme: "https", Host: "8.8.8.8:5000", Path: "/v2/team/image/blobs/sha256:abc"},
				Header: http.Header{
					"Authorization":       []string{"Bearer source-token"},
					"Cookie":              []string{"session=source"},
					"Proxy-Authorization": []string{"Basic cHJveHktc2VjcmV0"},
				},
			}).WithContext(ctx)
			previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example:5000"}}}
			if err := client.checkRedirect(request, previous); err != nil {
				t.Fatalf("cross-host redirect rejected: %v", err)
			}
			if got := request.Header.Get("Authorization"); got != test.wantAuth {
				t.Fatalf("target Authorization = %q, want %q", got, test.wantAuth)
			}
			for _, name := range []string{"Cookie", "Proxy-Authorization"} {
				if got := request.Header.Get(name); got != "" {
					t.Errorf("redirect retained source %s %q", name, got)
				}
			}
		})
	}
}

func TestRedirectRejectsPrivateTargetAddress(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{
		"127.0.0.1": {Ports: []uint16{443}},
	}}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "8.8.8.8")
	request := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "127.0.0.1", Path: "/blob"},
		Header: make(http.Header),
	}).WithContext(ctx)
	previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "8.8.8.8"}}}
	if err := client.checkRedirect(request, previous); !errors.Is(err, ErrSSRFRejected) {
		t.Fatalf("redirect to loopback error = %v, want SSRF rejection", err)
	}
}
