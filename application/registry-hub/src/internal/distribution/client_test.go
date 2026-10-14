package distribution

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestRequestEscapesRegistryReferenceOnce(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	client := &Client{base: &url.URL{Scheme: "https", Host: "registry.test"}}
	request, err := client.request(context.Background(), "GET", "/v2/team/image/manifests/"+url.PathEscape(digest), nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "/v2/team/image/manifests/" + url.PathEscape(digest)
	if request.URL.EscapedPath() != want {
		t.Fatalf("escaped path = %q, want %q", request.URL.EscapedPath(), want)
	}
	if strings.Contains(request.URL.EscapedPath(), "%25") {
		t.Fatalf("path was encoded twice: %q", request.URL.EscapedPath())
	}
}

func TestEmptyUploadCommitDoesNotUseChunkedTransfer(t *testing.T) {
	var contentLength int64
	var transferEncoding []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		contentLength = request.ContentLength
		transferEncoding = append([]string(nil), request.TransferEncoding...)
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{base: endpoint, client: server.Client()}
	response, err := client.Do(context.Background(), http.MethodPut, "/v2/repository/blobs/uploads/session", url.Values{"digest": {"sha256:abc"}}, nil, strings.NewReader(""), 0)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload commit status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if contentLength != 0 || len(transferEncoding) != 0 {
		t.Fatalf("empty request had ContentLength=%d TransferEncoding=%v, want length 0 without chunked transfer", contentLength, transferEncoding)
	}
}

func TestStorageRedirectRequiresAllowlistedOrigin(t *testing.T) {
	allowed, _ := url.Parse("https://storage.example")
	client := &Client{redirectEndpoints: []*url.URL{allowed}}
	target, _ := url.Parse("https://storage.example/object")
	if !client.redirectEndpointAllowed(target) {
		t.Fatal("expected allowlisted storage origin")
	}
	other, _ := url.Parse("https://other.example/object")
	if client.redirectEndpointAllowed(other) {
		t.Fatal("unexpected redirect origin allowance")
	}
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	request.Header.Set("Authorization", "Bearer internal")
	request.Header.Set("Cookie", "session=secret")
	request.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	if err := client.checkRedirect(request, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("internal credentials were retained on storage redirect")
	}
}

func TestStorageRedirectFollowsAllowlistedTargetWithoutForwardingCredentials(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "registry.example"}
	allowed, _ := url.Parse("https://storage.example:9443")
	client := &Client{base: base, user: "distribution-user", password: "distribution-password", redirectEndpoints: []*url.URL{allowed}}
	observed := map[string]int{}
	client.SetObserver(func(event, result string) { observed[event+":"+result]++ })
	var storageRequest *http.Request
	client.client = &http.Client{Transport: distributionRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == base.Host {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"https://storage.example:9443/object?signature=private"}},
				Body:       io.NopCloser(strings.NewReader("")), Request: request,
			}, nil
		}
		storageRequest = request
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("blob")), Request: request}, nil
	})}
	client.client.CheckRedirect = client.checkRedirect

	response, err := client.Do(context.Background(), http.MethodGet, "/v2/team/image/blobs/sha256:abc", nil, http.Header{
		"Authorization":       []string{"Bearer client-secret"},
		"Cookie":              []string{"client-cookie=secret"},
		"Proxy-Authorization": []string{"Basic client-proxy-secret"},
	}, nil, 0)
	if err != nil {
		t.Fatalf("allowlisted redirect failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("redirect response status = %d, want 200", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "blob" {
		t.Fatalf("redirect response body = %q, error=%v; want the storage bytes", body, err)
	}
	if response.Header.Get("Location") != "" {
		t.Fatalf("storage redirect Location leaked to the client: %q", response.Header.Get("Location"))
	}
	if observed["redirect:accepted"] != 1 {
		t.Fatalf("accepted redirect observations = %d, want 1", observed["redirect:accepted"])
	}
	if storageRequest == nil {
		t.Fatal("allowlisted storage target was not requested")
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		if value := storageRequest.Header.Get(name); value != "" {
			t.Errorf("storage request retained %s: %q", name, value)
		}
	}
}

func TestDistributionAPIErrorObserverUsesBoundedStatus(t *testing.T) {
	client := &Client{base: &url.URL{Scheme: "https", Host: "registry.example"}}
	client.client = &http.Client{Transport: distributionRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	observed := map[string]int{}
	client.SetObserver(func(event, result string) { observed[event+":"+result]++ })
	response, err := client.Do(context.Background(), http.MethodGet, "/v2/", nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if observed["api_error:503"] != 1 {
		t.Fatalf("503 API error observations = %d, want 1", observed["api_error:503"])
	}
}

func TestStorageRedirectRejectsSchemeDowngradePortAndWriteMethod(t *testing.T) {
	allowed, _ := url.Parse("https://storage.example:9443")
	client := &Client{redirectEndpoints: []*url.URL{allowed}}
	for _, test := range []struct {
		name   string
		method string
		target string
	}{
		{name: "https downgrade", method: http.MethodGet, target: "http://storage.example:9443/object"},
		{name: "unlisted port", method: http.MethodGet, target: "https://storage.example:9444/object"},
		{name: "write method", method: http.MethodPut, target: "https://storage.example:9443/object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, _ := url.Parse(test.target)
			request := &http.Request{Method: test.method, URL: target, Header: make(http.Header)}
			via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}
			if err := client.checkRedirect(request, via); err == nil {
				t.Fatal("unsafe redirect was accepted")
			}
		})
	}
}

func TestStorageRedirectRevalidatesEveryHopAndCapsRedirects(t *testing.T) {
	allowed, _ := url.Parse("https://storage.example:9443")
	client := &Client{redirectEndpoints: []*url.URL{allowed}}
	first, _ := url.Parse("https://storage.example:9443/first")
	second, _ := url.Parse("https://storage.example:9443/second")
	third, _ := url.Parse("https://other.example/third")
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}, {URL: first}}
	request := &http.Request{Method: http.MethodGet, URL: second, Header: make(http.Header)}
	if err := client.checkRedirect(request, via); err != nil {
		t.Fatalf("valid first redirect hop failed: %v", err)
	}
	request.URL = third
	if err := client.checkRedirect(request, append(via, &http.Request{URL: second})); err == nil {
		t.Fatal("unlisted second redirect hop was accepted")
	}

	tooManyHops := make([]*http.Request, 5)
	for index := range tooManyHops {
		tooManyHops[index] = &http.Request{URL: first}
	}
	request.URL = second
	if err := client.checkRedirect(request, tooManyHops); err == nil {
		t.Fatal("redirect beyond the hop limit was accepted")
	}
}

func TestStorageRedirectAddressPolicyRejectsPrivateAndMetadataRanges(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.0.2.1", "::1", "fd00::1"} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !unsafeRedirectAddress(address) {
			t.Errorf("unsafe redirect address %s was allowed", raw)
		}
	}
	public, _ := netip.ParseAddr("8.8.8.8")
	if unsafeRedirectAddress(public) {
		t.Fatal("public unicast address was rejected")
	}
}

type distributionRoundTripper func(*http.Request) (*http.Response, error)

func (f distributionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestStorageRedirectIsRejectedWithoutAllowlist(t *testing.T) {
	client := &Client{}
	target, _ := url.Parse("https://storage.example/object")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	if err := client.checkRedirect(request, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}); err == nil {
		t.Fatal("redirect without an allowlist was accepted")
	}
}

func TestStorageRedirectRejectsUserInfo(t *testing.T) {
	allowed, _ := url.Parse("https://storage.example")
	client := &Client{redirectEndpoints: []*url.URL{allowed}}
	target, _ := url.Parse("https://user:password@storage.example/object")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	if err := client.checkRedirect(request, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}); err == nil {
		t.Fatal("redirect userinfo was accepted")
	}
}

func TestStorageRedirectRejectsLoopbackTarget(t *testing.T) {
	allowed, _ := url.Parse("https://127.0.0.1")
	client := &Client{redirectEndpoints: []*url.URL{allowed}}
	target, _ := url.Parse("https://127.0.0.1/object")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	if err := client.checkRedirect(request, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}); err == nil {
		t.Fatal("loopback redirect target was accepted")
	}
}
