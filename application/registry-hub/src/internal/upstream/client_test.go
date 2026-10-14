package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"registry-hub/internal/config"
	"strings"
	"testing"
)

func TestInitialURLEscapesRegistryReferenceOnce(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	client := &Client{rules: map[string]config.TransportRule{}}
	requestURL, err := client.initialURL("registry.example", "/v2/team/image/manifests/"+url.PathEscape(digest))
	if err != nil {
		t.Fatal(err)
	}
	want := "/v2/team/image/manifests/" + url.PathEscape(digest)
	if requestURL.EscapedPath() != want {
		t.Fatalf("escaped path = %q, want %q", requestURL.EscapedPath(), want)
	}
	if strings.Contains(requestURL.EscapedPath(), "%25") {
		t.Fatalf("path was encoded twice: %q", requestURL.EscapedPath())
	}
}

func TestInitialURLUsesCaseInsensitiveTransportRule(t *testing.T) {
	client, err := New(config.Config{TransportRules: map[string]config.TransportRule{
		"REGISTRY.EXAMPLE:80": {AllowHTTP: true, Ports: []uint16{80}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	requestURL, err := client.initialURL("registry.example:80", "/v2/team/image/blobs/sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if requestURL.Scheme != "http" {
		t.Fatalf("scheme = %q, want http", requestURL.Scheme)
	}
}

func TestDoDoesNotFallbackFromHeadWhenUpstreamReturns405(t *testing.T) {
	var methods []string
	client := &Client{
		rules: map[string]config.TransportRule{},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			methods = append(methods, request.Method)
			return &http.Response{
				StatusCode: http.StatusMethodNotAllowed,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("method not allowed")),
				Request:    request,
			}, nil
		})},
	}

	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodHead, "/v2/team/image/manifests/latest", nil, nil)
	if err != nil {
		t.Fatalf("HEAD request returned an error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Fatalf("upstream methods = %v, want one HEAD request", methods)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUpstreamTLSIsVerifiedByDefaultAndCallerAuthorizationIsNotForwarded(t *testing.T) {
	var captured *http.Request
	client, err := New(config.Config{})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	if client.transport.TLSClientConfig == nil || !client.transport.TLSClientConfig.InsecureSkipVerify || client.transport.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("upstream TLS verification callback is not configured")
	}
	requestURL, err := client.initialURL("registry.example", "/v2/team/image/manifests/latest")
	if err != nil {
		t.Fatalf("create default upstream URL: %v", err)
	}
	if requestURL.Scheme != "https" {
		t.Fatalf("default upstream scheme = %q, want https", requestURL.Scheme)
	}

	client.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured = request
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})}
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, http.Header{
			"Authorization":       {"Bearer client-secret"},
			"Cookie":              {"client-cookie=secret"},
			"Proxy-Authorization": {"Basic cHJveHktc2VjcmV0"},
		})
	if err != nil {
		t.Fatalf("upstream GET returned an error: %v", err)
	}
	defer response.Body.Close()
	if captured == nil || captured.Header.Get("Authorization") != "" || captured.Header.Get("Cookie") != "" || captured.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("caller credentials were forwarded upstream: %#v", captured)
	}
}

func TestClientTLSConfigUsesAuthorityCAAndValidatesHostname(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	certificate := server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	if len(certificate.DNSNames) == 0 {
		t.Fatal("test certificate has no DNS SAN")
	}
	serverName := certificate.DNSNames[0]
	state := tls.ConnectionState{ServerName: serverName, PeerCertificates: []*x509.Certificate{certificate}}

	clientTLS := newClientTLSConfig(map[string]*tls.Config{serverName: {RootCAs: roots}})
	if err := clientTLS.VerifyConnection(state); err != nil {
		t.Fatalf("certificate trusted for configured authority was rejected: %v", err)
	}
	state.ServerName = "untrusted.invalid"
	if err := clientTLS.VerifyConnection(state); err == nil {
		t.Fatal("certificate with a mismatched hostname was accepted")
	}
	if err := newClientTLSConfig(nil).VerifyConnection(tls.ConnectionState{
		ServerName: serverName, PeerCertificates: []*x509.Certificate{certificate},
	}); err == nil {
		t.Fatal("untrusted certificate was accepted without an authority CA")
	}
}

func TestClientRejectsInvalidAuthorityCAConfiguration(t *testing.T) {
	missing := t.TempDir() + "/missing.pem"
	invalid := t.TempDir() + "/invalid.pem"
	if err := os.WriteFile(invalid, []byte("not a PEM certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{missing, invalid} {
		t.Run(path, func(t *testing.T) {
			client, err := New(config.Config{TransportRules: map[string]config.TransportRule{
				"registry.example": {CAFile: path},
			}})
			if client != nil {
				client.Close()
			}
			if err == nil {
				t.Fatalf("New() accepted invalid CA file %q", path)
			}
		})
	}
}
