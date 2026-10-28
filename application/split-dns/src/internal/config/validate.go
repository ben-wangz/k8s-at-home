package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

var proxyNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func validateGroup(group UpstreamGroup, proxies map[string]Proxy) error {
	queryTimeout, err := time.ParseDuration(group.QueryTimeout)
	if err != nil || queryTimeout <= 0 {
		return errors.New("queryTimeout must be a positive duration")
	}
	if group.MaxConcurrentRequests < 1 {
		return errors.New("maxConcurrentRequests must be positive")
	}
	switch group.Type {
	case "forward":
		if len(group.Servers) == 0 || len(group.Endpoints) != 0 {
			return errors.New("forward groups require servers and cannot contain DoH endpoints")
		}
		if group.Strategy != "" || group.ConnectTimeout != "" || group.TLSHandshakeTimeout != "" || group.MaxAttemptsPerEndpoint != 0 {
			return errors.New("forward groups do not support DoH settings")
		}
		for _, server := range group.Servers {
			if err := validateServerAddress(server); err != nil {
				return err
			}
		}
		if group.Egress.Mode != "" || group.Egress.ProxyRef != "" {
			return errors.New("forward groups do not support egress settings")
		}
	case "doh":
		if len(group.Endpoints) == 0 || len(group.Servers) != 0 {
			return errors.New("DoH groups require endpoints and cannot contain forward servers")
		}
		if group.Strategy != "parallel" {
			return errors.New("DoH strategy must be parallel")
		}
		if group.MaxAttemptsPerEndpoint < 1 {
			return errors.New("maxAttemptsPerEndpoint must be at least one")
		}
		connectTimeout, err := time.ParseDuration(group.ConnectTimeout)
		if err != nil || connectTimeout <= 0 || connectTimeout > queryTimeout {
			return errors.New("connectTimeout must be positive and no greater than queryTimeout")
		}
		handshakeTimeout, err := time.ParseDuration(group.TLSHandshakeTimeout)
		if err != nil || handshakeTimeout <= 0 || handshakeTimeout > queryTimeout {
			return errors.New("tlsHandshakeTimeout must be positive and no greater than queryTimeout")
		}
		if group.Egress.Mode != "direct" && group.Egress.Mode != "proxy" {
			return errors.New("DoH egress mode must be direct or proxy")
		}
		if group.Egress.Mode == "direct" && group.Egress.ProxyRef != "" {
			return errors.New("direct DoH egress cannot reference a proxy")
		}
		if group.Egress.Mode == "proxy" {
			if group.Egress.ProxyRef == "" {
				return errors.New("proxy DoH egress requires proxyRef")
			}
			if _, ok := proxies[group.Egress.ProxyRef]; !ok {
				return fmt.Errorf("references missing proxy %q", group.Egress.ProxyRef)
			}
		}
		for i, endpoint := range group.Endpoints {
			if err := validateEndpoint(endpoint); err != nil {
				return fmt.Errorf("endpoint %d: %w", i, err)
			}
		}
	default:
		return errors.New("type must be forward or doh")
	}
	return nil
}

func validateEndpoint(endpoint Endpoint) error {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("URL must be an HTTPS URL without user info or fragment")
	}
	for _, address := range endpoint.BootstrapIPs {
		if net.ParseIP(address) == nil {
			return fmt.Errorf("bootstrap address %q is not an IP address", address)
		}
	}
	return nil
}

func validateProxy(proxy Proxy) error {
	parsed, err := url.Parse(proxy.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL must be an HTTP proxy URL without path, query, fragment, or user info")
	}
	if parsed.Hostname() == "" {
		return errors.New("proxy URL must contain a hostname or IP address")
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("proxy URL contains an invalid port")
		}
	}
	if (proxy.UsernameEnv == "") != (proxy.PasswordEnv == "") {
		return errors.New("usernameEnv and passwordEnv must be configured together")
	}
	if proxy.UsernameEnv != "" {
		if _, ok := os.LookupEnv(proxy.UsernameEnv); !ok {
			return fmt.Errorf("username environment variable %q is not set", proxy.UsernameEnv)
		}
		if _, ok := os.LookupEnv(proxy.PasswordEnv); !ok {
			return fmt.Errorf("password environment variable %q is not set", proxy.PasswordEnv)
		}
	}
	return nil
}

func validateServerAddress(value string) error {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || net.ParseIP(host) == nil {
		return fmt.Errorf("server %q must be an IP address and port", value)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("server %q has an invalid port", value)
	}
	return nil
}
