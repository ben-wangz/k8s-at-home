package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

func (c *Client) setAuthorityCredential(request *http.Request, authority, targetHost string) {
	credential, ok := c.auth[authority]
	if !ok {
		for configuredAuthority, configured := range c.auth {
			if strings.EqualFold(configuredAuthority, authority) {
				credential, ok = configured, true
				break
			}
		}
	}
	if !ok && strings.EqualFold(authority, "registry-1.docker.io") {
		credential, ok = c.auth["docker.io"]
	}
	if !ok {
		return
	}
	if !strings.EqualFold(targetHost, authority) && !(strings.EqualFold(authority, "docker.io") && (strings.EqualFold(targetHost, "registry-1.docker.io") || strings.EqualFold(targetHost, "auth.docker.io"))) {
		return
	}
	switch credential.Type {
	case "basic":
		request.SetBasicAuth(credential.Username, credential.Password)
	case "bearer":
		request.Header.Set("Authorization", "Bearer "+credential.Token)
	}
}

func (c *Client) obtainToken(ctx context.Context, authority, repository string, registryURL *url.URL, challenge map[string]string) (string, error) {
	realm, err := url.Parse(challenge["realm"])
	if err != nil || realm.User != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return "", errors.New("upstream Bearer realm is invalid")
	}
	allowed := strings.EqualFold(realm.Host, registryURL.Host)
	if strings.EqualFold(authority, "docker.io") && strings.EqualFold(realm.Hostname(), "auth.docker.io") {
		allowed = true
	}
	if !allowed {
		return "", errors.New("upstream Bearer realm is outside the authority allowlist")
	}
	if err := c.checkTarget(ctx, authority, realm, registryURL.Scheme); err != nil {
		return "", err
	}
	query := realm.Query()
	if service := challenge["service"]; service != "" {
		query.Set("service", service)
	}
	query.Set("scope", "repository:"+repository+":pull")
	realm.RawQuery = query.Encode()
	tokenAuthority := authority
	if !strings.EqualFold(realm.Host, registryURL.Host) && !isDockerAlias(realm) {
		tokenAuthority = realm.Host
	}
	tokenContext := context.WithValue(ctx, authorityContextKey{}, tokenAuthority)
	tokenContext = context.WithValue(tokenContext, transportAuthorityContextKey{}, realm.Host)
	request, err := http.NewRequestWithContext(tokenContext, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(realm.Host, registryURL.Host) || strings.EqualFold(authority, "docker.io") && strings.EqualFold(realm.Hostname(), "auth.docker.io") {
		c.setAuthorityCredential(request, authority, realm.Host)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &HTTPStatusError{StatusCode: response.StatusCode, RetryAfter: response.Header.Get("Retry-After")}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxTokenResponse {
		return "", errors.New("upstream token response exceeds limit")
	}
	var tokenResponse struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tokenResponse); err != nil {
		return "", errors.New("upstream token response is invalid")
	}
	if tokenResponse.Token == "" {
		tokenResponse.Token = tokenResponse.AccessToken
	}
	if tokenResponse.Token == "" {
		return "", errors.New("upstream token response did not contain a token")
	}
	return tokenResponse.Token, nil
}

func safeRequestHeaders(source http.Header) http.Header {
	result := make(http.Header)
	for _, name := range []string{"Accept", "Range", "If-None-Match", "If-Modified-Since"} {
		for _, value := range source.Values(name) {
			result.Add(name, value)
		}
	}
	result.Set("Accept-Encoding", "identity")
	return result
}

func parseBearerChallenge(headers []string) map[string]string {
	for _, header := range headers {
		lower := strings.ToLower(header)
		start := strings.Index(lower, "bearer ")
		if start < 0 || start > 0 && header[start-1] != ',' {
			continue
		}
		params := make(map[string]string)
		input := strings.TrimSpace(header[start+len("bearer "):])
		for len(input) > 0 {
			input = strings.TrimLeft(input, " ,\t")
			keyEnd := strings.IndexAny(input, "=,")
			if keyEnd < 1 || input[keyEnd] != '=' {
				break
			}
			key := strings.ToLower(strings.TrimSpace(input[:keyEnd]))
			input = strings.TrimSpace(input[keyEnd+1:])
			var value string
			if strings.HasPrefix(input, `"`) {
				input = input[1:]
				var out strings.Builder
				for i := 0; i < len(input); i++ {
					if input[i] == '\\' && i+1 < len(input) {
						i++
						out.WriteByte(input[i])
						continue
					}
					if input[i] == '"' {
						input = input[i+1:]
						break
					}
					out.WriteByte(input[i])
				}
				value = out.String()
			} else {
				end := strings.IndexByte(input, ',')
				if end < 0 {
					value, input = input, ""
				} else {
					value, input = input[:end], input[end:]
				}
			}
			params[key] = value
			if comma := strings.IndexByte(input, ','); comma >= 0 {
				input = input[comma+1:]
			} else {
				break
			}
		}
		if params["realm"] != "" {
			return params
		}
	}
	return nil
}

func (c *Client) resolvePublic(ctx context.Context, host string) ([]netip.Addr, error) {
	lookupIP := c.lookupIP
	if lookupIP == nil {
		lookupIP = net.DefaultResolver.LookupNetIP
	}
	return resolvePublicWith(ctx, host, lookupIP)
}

func resolvePublicWith(ctx context.Context, host string, lookupIP func(context.Context, string, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !isPublic(address) {
			return nil, rejectedByPolicy(ErrSSRFRejected, "upstream address is not public")
		}
		return []netip.Addr{address}, nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, rejectedByPolicy(ErrSSRFRejected, "loopback hostnames are forbidden")
	}
	resolved, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream host: %w", err)
	}
	if len(resolved) == 0 {
		return nil, errors.New("upstream DNS returned no addresses")
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	for _, address := range resolved {
		address = address.Unmap()
		if !isPublic(address) {
			return nil, rejectedByPolicy(ErrSSRFRejected, "upstream DNS returned a non-public address")
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func isPublic(address netip.Addr) bool {
	if !address.IsValid() || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, raw := range []string{
		"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "168.63.129.16/32",
		"2001:db8::/32", "2001::/23",
	} {
		prefix, _ := netip.ParsePrefix(raw)
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
