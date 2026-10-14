package testkit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func NewHTTPClient(timeout time.Duration, roots *x509.CertPool) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func Request(ctx context.Context, client *http.Client, method, endpoint string, body io.Reader, headers map[string]string, basic *BasicCredential) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if basic != nil {
		request.SetBasicAuth(basic.Username, basic.Password)
	}
	return client.Do(request)
}

func ExpectStatus(response *http.Response, expected ...int) error {
	defer response.Body.Close()
	for _, status := range expected {
		if response.StatusCode == status {
			return nil
		}
	}
	return fmt.Errorf("unexpected HTTP status %s", response.Status)
}

func ReadBody(response *http.Response, limit int64) ([]byte, error) {
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("HTTP response exceeded %d bytes", limit)
	}
	return content, nil
}

func ResolveLocation(base, location string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	locationURL, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	resolved := baseURL.ResolveReference(locationURL)
	if resolved.Host != baseURL.Host || resolved.Scheme != baseURL.Scheme {
		return "", fmt.Errorf("registry returned an upload URL outside the configured service")
	}
	return resolved.String(), nil
}

func RequireContentType(response *http.Response, expected string) error {
	actual := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if actual != expected {
		return fmt.Errorf("unexpected content type %q", actual)
	}
	return nil
}

type BasicCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
