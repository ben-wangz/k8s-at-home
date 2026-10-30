package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"registry-hub/internal/config"
	"registry-hub/internal/route"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxTokenResponse = 1 << 20

var (
	ErrSSRFRejected           = errors.New("upstream target rejected by SSRF policy")
	ErrSecurityPolicyRejected = errors.New("upstream target rejected by security policy")
)

func rejectedByPolicy(kind error, message string) error {
	return fmt.Errorf("%w: %s", kind, message)
}

type authorityContextKey struct{}
type transportAuthorityContextKey struct{}

type HTTPStatusError struct {
	StatusCode int
	RetryAfter string
}

func (e *HTTPStatusError) HTTPStatusCode() int { return e.StatusCode }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("upstream token endpoint returned status %d", e.StatusCode)
}

type Client struct {
	auth            map[string]config.UpstreamAuth
	rules           map[string]config.TransportRule
	caPools         map[string]*tls.Config
	proxies         config.ProxyConfig
	lookupIP        func(context.Context, string, string) ([]netip.Addr, error)
	transport       *http.Transport
	client          *http.Client
	proxyMu         sync.RWMutex
	proxiesByHost   map[string]struct{}
	observer        func(string)
	attemptObserver atomic.Pointer[HTTPAttemptObserver]
}

func New(cfg config.Config) (*Client, error) {
	auth := make(map[string]config.UpstreamAuth, len(cfg.UpstreamAuth))
	for authority, credential := range cfg.UpstreamAuth {
		auth[strings.ToLower(authority)] = credential
	}
	rules := make(map[string]config.TransportRule, len(cfg.TransportRules))
	for authority, rule := range cfg.TransportRules {
		rules[strings.ToLower(authority)] = rule
	}
	proxyConfig := config.ProxyConfig{Default: cfg.Proxy.Default, Authorities: make(map[string]string, len(cfg.Proxy.Authorities))}
	for authority, proxy := range cfg.Proxy.Authorities {
		proxyConfig.Authorities[strings.ToLower(authority)] = proxy
	}
	client := &Client{
		auth:          auth,
		rules:         rules,
		caPools:       make(map[string]*tls.Config),
		proxies:       proxyConfig,
		lookupIP:      net.DefaultResolver.LookupNetIP,
		proxiesByHost: make(map[string]struct{}),
	}
	for authority, rule := range cfg.TransportRules {
		if rule.CAFile == "" {
			continue
		}
		tlsConfig, err := config.LoadTLS(rule.CAFile)
		if err != nil {
			return nil, fmt.Errorf("load upstream CA for %s: %w", authority, err)
		}
		host := authorityHost(authority)
		client.caPools[strings.ToLower(strings.Trim(host, "[]"))] = tlsConfig
		if strings.EqualFold(authority, "docker.io") {
			client.caPools["registry-1.docker.io"] = tlsConfig
			client.caPools["auth.docker.io"] = tlsConfig
		}
	}
	transport := &http.Transport{
		Proxy:                 client.selectProxy,
		DialContext:           client.dialContext,
		TLSClientConfig:       newClientTLSConfig(client.caPools),
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client.transport = transport
	client.client = &http.Client{Transport: attemptTransport{base: transport, client: client}, CheckRedirect: client.checkRedirect}
	return client, nil
}

func newClientTLSConfig(caPools map[string]*tls.Config) *tls.Config {
	systemRoots, _ := x509.SystemCertPool()
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// VerifyConnection restores chain and hostname checks with authority-specific roots.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.ServerName == "" || len(state.PeerCertificates) == 0 {
				return errors.New("upstream TLS peer did not provide a verifiable certificate")
			}
			roots := systemRoots
			serverName := strings.TrimSuffix(strings.ToLower(state.ServerName), ".")
			if configured, ok := caPools[serverName]; ok && configured.RootCAs != nil {
				roots = configured.RootCAs
			}
			intermediates := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: intermediates,
				DNSName:       state.ServerName,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return fmt.Errorf("verify upstream TLS certificate: %w", err)
			}
			return nil
		},
	}
}

func (c *Client) Do(ctx context.Context, authority, repository, method, requestPath string, query url.Values, headers http.Header) (*http.Response, error) {
	if err := route.ValidateAuthority(authority); err != nil {
		return nil, route.ErrInvalidPath
	}
	requestURL, err := c.initialURL(authority, requestPath)
	if err != nil {
		return nil, err
	}
	if query != nil {
		requestURL.RawQuery = query.Encode()
	}
	if err := c.checkTarget(ctx, authority, requestURL, requestURL.Scheme); err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, authorityContextKey{}, authority)
	ctx = context.WithValue(ctx, transportAuthorityContextKey{}, requestURL.Host)
	ctx = c.attemptContext(ctx, authority, repository, requestPath)
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header = safeRequestHeaders(headers)
	c.setAuthorityCredential(request, authority, request.URL.Host)
	response, err := c.client.Do(request)
	if response != nil && response.StatusCode == http.StatusUnauthorized && parseBearerChallenge(response.Header.Values("WWW-Authenticate")) != nil {
		c.observe("upstream_auth_challenge")
	}
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	challenge := parseBearerChallenge(response.Header.Values("WWW-Authenticate"))
	if challenge == nil {
		return response, nil
	}
	effectiveURL := request.URL
	if response.Request != nil && response.Request.URL != nil {
		effectiveURL = response.Request.URL
	}
	tokenAuthority := effectiveURL.Host
	if strings.EqualFold(effectiveURL.Hostname(), "registry-1.docker.io") || strings.EqualFold(effectiveURL.Hostname(), "auth.docker.io") {
		tokenAuthority = "docker.io"
	}
	accessToken, tokenErr := c.obtainToken(ctx, tokenAuthority, repository, effectiveURL, challenge)
	if tokenErr != nil {
		_ = response.Body.Close()
		return nil, tokenErr
	}
	_ = response.Body.Close()
	retryURL := *effectiveURL
	retry, err := http.NewRequestWithContext(ctx, method, retryURL.String(), nil)
	if err != nil {
		return nil, err
	}
	retry.Header = safeRequestHeaders(headers)
	retry.Header.Set("Authorization", "Bearer "+accessToken)
	retry.Host = retryURL.Host
	return c.client.Do(retry)
}

func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}

func (c *Client) SetObserver(observer func(string)) {
	c.observer = observer
}

func (c *Client) observe(event string) {
	if c.observer != nil {
		c.observer(event)
	}
}

func Retryable(err error) bool {
	var statusError *HTTPStatusError
	if errors.As(err, &statusError) {
		return RetryableStatus(statusError.StatusCode)
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}
	var networkError net.Error
	if !errors.As(err, &networkError) {
		return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
	}
	return networkError.Timeout() || networkError.Temporary()
}

func RetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func RetryAfter(err error) string {
	var statusError *HTTPStatusError
	if errors.As(err, &statusError) {
		return statusError.RetryAfter
	}
	return ""
}
