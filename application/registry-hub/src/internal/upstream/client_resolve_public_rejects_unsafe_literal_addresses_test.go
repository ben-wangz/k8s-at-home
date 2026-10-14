package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"registry-hub/internal/config"
	"testing"
)

func TestResolvePublicRejectsUnsafeLiteralAddresses(t *testing.T) {
	client := &Client{}
	for _, address := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "168.63.129.16", "192.0.2.1", "198.18.0.1", "::1", "fc00::1", "fe80::1", "2001:db8::1",
		"::ffff:8.8.8.8",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := client.resolvePublic(context.Background(), address); err == nil {
				t.Fatalf("unsafe address %s was accepted", address)
			}
		})
	}
}

func TestTargetRequiresExplicitNonstandardPortRule(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{}}
	ctx := context.Background()
	target := &url.URL{Scheme: "https", Host: "8.8.8.8:8443"}
	if err := client.checkTarget(ctx, target.Host, target, "https"); err == nil {
		t.Fatal("nonstandard upstream port without an explicit rule was accepted")
	}
	client.rules[target.Host] = config.TransportRule{Ports: []uint16{8443}}
	if err := client.checkTarget(ctx, target.Host, target, "https"); err != nil {
		t.Fatalf("explicitly allowed public target port was rejected: %v", err)
	}
}

func TestResolvePublicRejectsMixedDNSAnswers(t *testing.T) {
	client := &Client{lookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.168.1.10")}, nil
	}}
	if _, err := client.resolvePublic(context.Background(), "registry.example"); err == nil {
		t.Fatal("DNS response containing both public and private addresses was accepted")
	}
}

func TestResolvePublicUnmapsIPv4MappedDNSAnswers(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
		wantErr bool
	}{
		{name: "public", address: "::ffff:34.192.215.158", want: "34.192.215.158"},
		{name: "private", address: "::ffff:10.1.2.3", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{lookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr(test.address)}, nil
			}}
			addresses, err := client.resolvePublic(context.Background(), "registry.example")
			if test.wantErr {
				if !errors.Is(err, ErrSSRFRejected) {
					t.Fatalf("private mapped DNS answer error = %v, want SSRF rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("public mapped DNS answer was rejected: %v", err)
			}
			if len(addresses) != 1 || addresses[0].String() != test.want || addresses[0].Is4In6() {
				t.Fatalf("normalized DNS addresses = %v, want [%s] as native IPv4", addresses, test.want)
			}
		})
	}
}

func TestDialContextRechecksDNSToRejectRebinding(t *testing.T) {
	lookups := 0
	client := &Client{lookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
	}}
	if _, err := client.resolvePublic(context.Background(), "registry.example"); err != nil {
		t.Fatalf("initial public DNS answer was rejected: %v", err)
	}
	if _, err := client.dialContext(context.Background(), "tcp", "registry.example:443"); err == nil {
		t.Fatal("connection-stage private DNS answer was accepted")
	}
	if lookups != 2 {
		t.Fatalf("DNS lookups = %d, want preflight and connection lookups", lookups)
	}
}

func TestRedirectDoesNotInheritHTTPPermissionAcrossHosts(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{
		"registry.example:80": {AllowHTTP: true, Ports: []uint16{80}},
	}}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "registry.example:80")
	request := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "8.8.8.8:80"},
		Header: make(http.Header),
	}).WithContext(ctx)
	previous := []*http.Request{{URL: &url.URL{Scheme: "http", Host: "registry.example:80"}}}
	if err := client.checkRedirect(request, previous); err == nil {
		t.Fatal("cross-host redirect unexpectedly inherited source HTTP permission")
	}
}

func TestRedirectRejectsNonHTTPAndExcessiveHops(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{
		"8.8.8.8": {AllowHTTP: true, Ports: []uint16{80}},
	}}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "registry.example")
	ctx = context.WithValue(ctx, transportAuthorityContextKey{}, "registry.example")
	previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry.example"}}}
	request := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "8.8.8.8"},
		Header: make(http.Header),
	}).WithContext(ctx)
	if err := client.checkRedirect(request, previous); err == nil {
		t.Fatal("HTTPS downgrade redirect was accepted")
	}
	request.URL = &url.URL{Scheme: "ftp", Host: "8.8.8.8"}
	if err := client.checkRedirect(request, previous); err == nil {
		t.Fatal("non-HTTP redirect scheme was accepted")
	}

	tooManyHops := make([]*http.Request, 5)
	for index := range tooManyHops {
		tooManyHops[index] = &http.Request{URL: &url.URL{Scheme: "https", Host: "registry.example"}}
	}
	request.URL = &url.URL{Scheme: "https", Host: "registry.example"}
	if err := client.checkRedirect(request, tooManyHops); err == nil {
		t.Fatal("redirect beyond the hop limit was accepted")
	}
}

func TestRedirectRejectsNonstandardDockerAliasPort(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{}}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "docker.io")
	request := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "registry-1.docker.io:5000"},
		Header: make(http.Header),
	}).WithContext(ctx)
	previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "registry-1.docker.io"}}}
	if err := client.checkRedirect(request, previous); err == nil {
		t.Fatal("Docker Hub alias unexpectedly allowed a nonstandard port")
	}
}

func TestRedirectUsesLogicalAuthorityForSameHost(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{
		"registry.example:5000": {Ports: []uint16{5000}},
	}}
	ctx := context.WithValue(context.Background(), authorityContextKey{}, "registry.example:5000")
	request := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "8.8.8.8:5000"},
		Header: make(http.Header),
	}).WithContext(ctx)
	previous := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "8.8.8.8:5000"}}}
	if err := client.checkRedirect(request, previous); err != nil {
		t.Fatalf("same-host redirect should use logical authority policy: %v", err)
	}
}
