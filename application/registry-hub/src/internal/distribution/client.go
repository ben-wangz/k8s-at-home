package distribution

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"registry-hub/internal/config"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base              *url.URL
	user              string
	password          string
	token             string
	redirectEndpoints []*url.URL
	client            *http.Client
	transport         *http.Transport
	observer          func(string, string)
}

func New(cfg config.Config) (*Client, error) {
	tlsConfig, err := config.LoadTLS(cfg.DistributionCA)
	if err != nil {
		return nil, fmt.Errorf("load Distribution CA: %w", err)
	}
	redirectEndpoints := make([]*url.URL, 0, len(cfg.DistributionRedirectEndpoints))
	for _, endpoint := range cfg.DistributionRedirectEndpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("parse Distribution redirect endpoint: %w", err)
		}
		redirectEndpoints = append(redirectEndpoints, parsed)
	}
	client := &Client{
		base: cfg.DistributionURL, user: cfg.DistributionAuth.Username,
		password: cfg.DistributionAuth.Password, token: cfg.DistributionAuth.Token,
		redirectEndpoints: redirectEndpoints,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           client.dialContext,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client.transport = transport
	client.client = &http.Client{Transport: transport, CheckRedirect: client.checkRedirect}
	return client, nil
}

func (c *Client) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(c.redirectEndpoints) == 0 {
		return errors.New("Distribution storage redirect is disabled")
	}
	if request.URL.User != nil || request.URL.Fragment != "" {
		return errors.New("Distribution storage redirect contains credentials or a fragment")
	}
	if err := validateRedirectHost(request.URL); err != nil {
		return err
	}
	if len(via) >= 5 || request.Method != http.MethodGet && request.Method != http.MethodHead {
		return errors.New("Distribution storage redirect is not allowed for this request")
	}
	if len(via) > 0 && strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(request.URL.Scheme, "https") {
		return errors.New("Distribution storage redirect HTTPS downgrade is forbidden")
	}
	if !c.redirectEndpointAllowed(request.URL) {
		return errors.New("Distribution storage redirect target is not allowlisted")
	}
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
	request.Header.Del("Proxy-Authorization")
	request.Host = request.URL.Host
	c.observe("redirect", "accepted")
	return nil
}

func (c *Client) SetObserver(observer func(string, string)) {
	c.observer = observer
}

func (c *Client) observe(event, result string) {
	if c.observer != nil {
		c.observer(event, result)
	}
}

func (c *Client) observeResponse(response *http.Response, err error) {
	if err != nil {
		c.observe("api_error", "transport_error")
		return
	}
	if response != nil && response.StatusCode >= 400 {
		c.observe("api_error", strconv.Itoa(response.StatusCode))
	}
}

func validateRedirectHost(target *url.URL) error {
	host := strings.TrimSpace(target.Hostname())
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("Distribution storage redirect target host is not allowed")
	}
	if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && unsafeRedirectAddress(address) {
		return errors.New("Distribution storage redirect target address is not allowed")
	}
	return nil
}

func unsafeRedirectAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || address.IsLinkLocalUnicast() || address.IsMulticast() {
		return true
	}
	for _, raw := range []string{
		"100.64.0.0/10", "169.254.169.254/32", "169.254.170.2/32", "192.0.0.0/24",
		"192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "168.63.129.16/32", "2001:db8::/32", "2001::/23",
	} {
		prefix, _ := netip.ParsePrefix(raw)
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (c *Client) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !c.redirectHostnameAllowed(host) {
		return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, errors.New("Distribution storage redirect target host is not allowed")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve Distribution storage redirect target: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("Distribution storage redirect DNS returned no addresses")
	}
	allowed := make(map[netip.Addr]struct{}, len(addresses))
	for _, target := range addresses {
		if unsafeRedirectAddress(target) {
			return nil, errors.New("Distribution storage redirect DNS returned a forbidden address")
		}
		allowed[target] = struct{}{}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for target := range allowed {
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(target.String(), port))
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		peerHost, _, splitErr := net.SplitHostPort(connection.RemoteAddr().String())
		peer, parseErr := netip.ParseAddr(strings.Split(peerHost, "%")[0])
		if splitErr != nil || parseErr != nil || unsafeRedirectAddress(peer) {
			_ = connection.Close()
			return nil, errors.New("Distribution storage redirect connected to a forbidden address")
		}
		if _, ok := allowed[peer]; !ok {
			_ = connection.Close()
			return nil, errors.New("Distribution storage redirect peer does not match DNS")
		}
		return connection, nil
	}
	if lastErr == nil {
		lastErr = errors.New("Distribution storage redirect target could not be reached")
	}
	return nil, lastErr
}

func (c *Client) redirectHostnameAllowed(host string) bool {
	host = strings.Trim(host, "[]")
	for _, endpoint := range c.redirectEndpoints {
		if strings.EqualFold(endpoint.Hostname(), host) {
			return true
		}
	}
	return false
}

func (c *Client) redirectEndpointAllowed(target *url.URL) bool {
	for _, endpoint := range c.redirectEndpoints {
		if !strings.EqualFold(endpoint.Scheme, target.Scheme) || !strings.EqualFold(endpoint.Hostname(), target.Hostname()) {
			continue
		}
		return effectivePort(endpoint) == effectivePort(target)
	}
	return false
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	return "80"
}
