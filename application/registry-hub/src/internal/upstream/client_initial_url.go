package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func (c *Client) initialURL(authority, requestPath string) (*url.URL, error) {
	if !strings.HasPrefix(requestPath, "/v2/") || strings.ContainsAny(requestPath, "\r\n") {
		return nil, errors.New("invalid upstream Registry path")
	}
	decodedPath, err := url.PathUnescape(requestPath)
	if err != nil || strings.ContainsAny(decodedPath, "\r\n") {
		return nil, errors.New("invalid upstream Registry path")
	}
	scheme := "https"
	if c.rules[strings.ToLower(authority)].AllowHTTP {
		scheme = "http"
	}
	host := authority
	if strings.EqualFold(authority, "docker.io") {
		host = "registry-1.docker.io"
	}
	return &url.URL{Scheme: scheme, Host: host, Path: decodedPath}, nil
}

func (c *Client) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream redirect limit exceeded")
	}
	previous := via[len(via)-1].URL
	if strings.EqualFold(previous.Scheme, "https") && strings.EqualFold(request.URL.Scheme, "http") {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream HTTPS downgrade is forbidden")
	}
	logicalAuthority, _ := request.Context().Value(authorityContextKey{}).(string)
	policyAuthority := c.redirectPolicyAuthority(logicalAuthority, via[0].URL, request.URL)
	if err := c.checkTarget(request.Context(), policyAuthority, request.URL, previous.Scheme); err != nil {
		return err
	}
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
	request.Header.Del("Proxy-Authorization")
	request.Host = request.URL.Host
	if strings.EqualFold(request.URL.Host, via[0].URL.Host) {
		c.setAuthorityCredential(request, logicalAuthority, request.URL.Host)
	} else if strings.EqualFold(logicalAuthority, "docker.io") &&
		(strings.EqualFold(request.URL.Hostname(), "registry-1.docker.io") || strings.EqualFold(request.URL.Hostname(), "auth.docker.io")) {
		// Docker Hub's registry and token hosts are the only cross-host aliases
		// allowed to receive credentials for the logical docker.io authority.
		c.setAuthorityCredential(request, logicalAuthority, request.URL.Host)
	} else {
		// A redirected registry may have its own independently configured
		// credential. Never carry the source authority's credential across hosts.
		c.setAuthorityCredential(request, request.URL.Host, request.URL.Host)
	}
	c.observe("upstream_redirect")
	return nil
}

func (c *Client) redirectPolicyAuthority(logicalAuthority string, initial, target *url.URL) string {
	if initial != nil && target != nil && strings.EqualFold(initial.Host, target.Host) {
		return logicalAuthority
	}
	if strings.EqualFold(logicalAuthority, "docker.io") && isDockerAlias(target) {
		return logicalAuthority
	}
	if target != nil {
		return target.Host
	}
	return logicalAuthority
}

func isDockerAlias(target *url.URL) bool {
	if target == nil {
		return false
	}
	return strings.EqualFold(target.Hostname(), "registry-1.docker.io") || strings.EqualFold(target.Hostname(), "auth.docker.io")
}

func (c *Client) checkTarget(ctx context.Context, policyAuthority string, target *url.URL, previousScheme string) error {
	if target == nil || target.User != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream target URL is not allowed")
	}
	if strings.EqualFold(previousScheme, "https") && target.Scheme == "http" {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream HTTPS downgrade is forbidden")
	}
	rule := c.rules[strings.ToLower(policyAuthority)]
	if target.Scheme == "http" && !rule.AllowHTTP {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream HTTP is not explicitly enabled")
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream port is invalid")
	}
	if len(rule.Ports) > 0 {
		allowed := false
		for _, candidate := range rule.Ports {
			if int(candidate) == portNumber {
				allowed = true
				break
			}
		}
		if !allowed {
			return rejectedByPolicy(ErrSecurityPolicyRejected, "upstream port is not allowed")
		}
	} else if portNumber != 443 && portNumber != 80 {
		return rejectedByPolicy(ErrSecurityPolicyRejected, "nonstandard upstream port requires explicit configuration")
	}
	_, err = c.resolvePublic(ctx, target.Hostname())
	return err
}

func authorityHost(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return strings.Trim(authority, "[]")
}

func (c *Client) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c.proxyMu.RLock()
	_, isProxy := c.proxiesByHost[canonicalDialAddress(address)]
	c.proxyMu.RUnlock()
	if isProxy {
		return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := c.resolvePublic(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, targetIP := range addresses {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(targetIP.String(), port))
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		peerHost, _, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
		peerIP, parseErr := netip.ParseAddr(strings.Split(peerHost, "%")[0])
		if splitErr != nil || parseErr != nil || !isPublic(peerIP) {
			_ = conn.Close()
			return nil, rejectedByPolicy(ErrSSRFRejected, "connected upstream peer IP is not public")
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = errors.New("upstream DNS returned no addresses")
	}
	return nil, lastErr
}

func (c *Client) selectProxy(request *http.Request) (*url.URL, error) {
	port := request.URL.Port()
	if port == "" {
		if request.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if noProxyMatch(request.URL.Hostname(), port, os.Getenv("NO_PROXY"), os.Getenv("no_proxy")) {
		return nil, nil
	}
	if environmentProxy, err := http.ProxyFromEnvironment(request); err != nil || environmentProxy != nil {
		if err != nil {
			return nil, err
		}
		c.markProxy(environmentProxy)
		return environmentProxy, nil
	}
	authority, _ := request.Context().Value(authorityContextKey{}).(string)
	if transportAuthority, _ := request.Context().Value(transportAuthorityContextKey{}).(string); transportAuthority != "" && !strings.EqualFold(request.URL.Host, transportAuthority) {
		if !strings.EqualFold(authority, "docker.io") || !isDockerAlias(request.URL) {
			authority = request.URL.Host
		}
	}
	proxyAddress := c.proxies.Authorities[strings.ToLower(authority)]
	if proxyAddress == "" {
		proxyAddress = c.proxies.Default
	}
	if proxyAddress == "" {
		return nil, nil
	}
	proxy, err := url.Parse(proxyAddress)
	if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") {
		return nil, errors.New("configured upstream proxy URL is invalid")
	}
	c.markProxy(proxy)
	return proxy, nil
}

func (c *Client) markProxy(proxy *url.URL) {
	if proxy == nil {
		return
	}
	port := proxy.Port()
	if port == "" {
		if proxy.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(proxy.Hostname(), port)
	c.proxyMu.Lock()
	c.proxiesByHost[canonicalDialAddress(address)] = struct{}{}
	c.proxyMu.Unlock()
}

func canonicalDialAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return strings.ToLower(strings.Trim(address, "[]"))
	}
	return strings.ToLower(net.JoinHostPort(strings.Trim(host, "[]"), port))
}
