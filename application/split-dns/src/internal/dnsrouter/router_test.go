package dnsrouter

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"split-dns/internal/config"
)

func TestSelectGroupBoundariesAndSpecificity(t *testing.T) {
	cfg := baseConfig()
	cfg.DNS.UpstreamGroups["specific"] = forwardGroup()
	cfg.DNS.UpstreamGroups["exact"] = forwardGroup()
	cfg.DNS.Rules = []config.Rule{
		{Match: "suffix", Domain: "example.com", UpstreamGroup: "specific"},
		{Match: "suffix", Domain: "foo.example.com", UpstreamGroup: "specific"},
		{Match: "exact", Domain: "foo.example.com", UpstreamGroup: "exact"},
	}
	router := newRouter(t, cfg)
	tests := map[string]string{
		"example.com":       "specific",
		"a.example.com":     "specific",
		"foo.example.com":   "exact",
		"x.foo.example.com": "specific",
		"notexample.com":    "default",
	}
	for name, want := range tests {
		if got := router.selectGroup(name); got != want {
			t.Errorf("selectGroup(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestForwardRetriesTruncatedUDPOverTCP(t *testing.T) {
	var udpRequests, tcpRequests atomic.Int32
	address := startDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		if writer.LocalAddr().Network() == "udp" {
			udpRequests.Add(1)
			response.Truncated = true
		} else {
			tcpRequests.Add(1)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   net.ParseIP("192.0.2.8").To4(),
			}}
		}
		_ = writer.WriteMsg(response)
	}))
	router := newRouter(t, baseConfig())
	group := &runtimeGroup{name: "default", forward: []string{address}}
	request := new(dns.Msg)
	request.SetQuestion("test.example.", dns.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := router.resolveForward(ctx, group, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answer) != 1 || udpRequests.Load() != 1 || tcpRequests.Load() != 1 {
		t.Fatalf("response=%v, udp=%d, tcp=%d", response.Answer, udpRequests.Load(), tcpRequests.Load())
	}
}

func TestDoHUsesBootstrapAndPreservesTLSHostname(t *testing.T) {
	server, caFile := startDoHServer(t, http.HandlerFunc(writeDNSAnswer))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpointURL := fmt.Sprintf("https://example.com:%s/dns-query", target.Port())
	cfg := dohConfig(endpointURL, []string{"127.0.0.1"}, caFile)
	router := newRouter(t, cfg)
	response, err := router.resolveDoH(context.Background(), router.groups["trusted"], query("registry.example."))
	if err != nil {
		t.Fatal(err)
	}
	if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 {
		t.Fatalf("DoH response = %#v", response)
	}
}

func TestDoHRejectsCertificateHostnameMismatch(t *testing.T) {
	server, caFile := startDoHServer(t, http.HandlerFunc(writeDNSAnswer))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Host = strings.Replace(parsed.Host, "127.0.0.1", "wrong.test", 1)
	cfg := dohConfig(parsed.String()+"/dns-query", []string{"127.0.0.1"}, caFile)
	router := newRouter(t, cfg)
	_, err = router.resolveDoH(context.Background(), router.groups["trusted"], query("registry.example."))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") && !strings.Contains(strings.ToLower(err.Error()), "valid for") {
		t.Fatalf("resolveDoH() error = %v, want certificate hostname error", err)
	}
}

func TestDoHUsesAuthenticatedHTTPConnectProxy(t *testing.T) {
	server, caFile := startDoHServer(t, http.HandlerFunc(writeDNSAnswer))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-pass"))
	var connectCount atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect || request.Header.Get("Proxy-Authorization") != proxyAuth {
			http.Error(writer, "proxy authentication failed", http.StatusProxyAuthRequired)
			return
		}
		connectCount.Add(1)
		upstream, err := net.DialTimeout("tcp", request.Host, time.Second)
		if err != nil {
			http.Error(writer, "connect failed", http.StatusBadGateway)
			return
		}
		client, _, err := http.NewResponseController(writer).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
			_ = client.Close()
		}()
		go func() {
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			_ = upstream.Close()
		}()
	}))
	defer proxy.Close()
	t.Setenv("SPLIT_DNS_TEST_PROXY_USER", "proxy-user")
	t.Setenv("SPLIT_DNS_TEST_PROXY_PASSWORD", "proxy-pass")
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("https://example.com:%s/dns-query", target.Port())
	cfg := dohConfig(endpoint, []string{"127.0.0.1"}, caFile)
	cfg.DNS.Proxies = map[string]config.Proxy{
		"test-proxy": {URL: proxyURL.String(), UsernameEnv: "SPLIT_DNS_TEST_PROXY_USER", PasswordEnv: "SPLIT_DNS_TEST_PROXY_PASSWORD"},
	}
	group := cfg.DNS.UpstreamGroups["trusted"]
	group.Egress = config.Egress{Mode: "proxy", ProxyRef: "test-proxy"}
	cfg.DNS.UpstreamGroups["trusted"] = group
	router := newRouter(t, cfg)
	response, err := router.resolveDoH(context.Background(), router.groups["trusted"], query("registry.example."))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answer) != 1 || connectCount.Load() == 0 {
		t.Fatalf("answer=%v, CONNECT requests=%d", response.Answer, connectCount.Load())
	}
}

func TestCachePreservesIndependentTTLsAndNegativeBound(t *testing.T) {
	settings := config.Cache{Enabled: true, MaxEntries: 4, MaxTTL: 300, NegativeMaxTTL: 30}
	cache := newResponseCache(settings)
	request := query("alias.example.")
	response := new(dns.Msg)
	response.SetReply(request)
	response.Answer = []dns.RR{
		&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 43}, Target: "target.example."},
		&dns.A{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.ParseIP("192.0.2.8").To4()},
	}
	cache.store("default", request, response)
	key, _ := cacheKey("default", request)
	entry := cache.entries[key]
	entry.inserted = time.Now().Add(-1900 * time.Millisecond)
	cache.entries[key] = entry
	lookup := query("ALIAS.EXAMPLE.")
	lookup.Id++
	hit, ok := cache.lookup("default", lookup)
	if !ok || hit.Id != lookup.Id || hit.Answer[0].Header().Ttl != 41 || hit.Answer[1].Header().Ttl != 298 {
		t.Fatalf("cache hit=%v, ok=%t", hit, ok)
	}

	negative := new(dns.Msg)
	negative.SetReply(query("missing.example."))
	negative.Rcode = dns.RcodeNameError
	negative.Ns = []dns.RR{&dns.SOA{
		Hdr:     dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
		Minttl:  60,
		Ns:      "ns.example.",
		Mbox:    "hostmaster.example.",
		Serial:  1,
		Refresh: 1,
		Retry:   1,
		Expire:  1,
	}}
	negativeTTL, cacheable := responseTTL(negative, 30, 300)
	if !cacheable || negativeTTL != 30 || negative.Ns[0].Header().Ttl != 30 {
		t.Fatalf("negative TTL=%d, cacheable=%t, SOA TTL=%d", negativeTTL, cacheable, negative.Ns[0].Header().Ttl)
	}
}

func TestCacheDoesNotExtendZeroTTL(t *testing.T) {
	cache := newResponseCache(config.Cache{Enabled: true, MaxEntries: 2, MaxTTL: 300, NegativeMaxTTL: 30})
	request := query("zero.example.")
	response := new(dns.Msg)
	response.SetReply(request)
	response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "zero.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.ParseIP("192.0.2.9").To4()}}
	cache.store("default", request, response)
	if len(cache.entries) != 0 {
		t.Fatalf("zero-TTL answer was cached: %d entries", len(cache.entries))
	}
}

func baseConfig() config.Config {
	var cfg config.Config
	cfg.ListenAddress = "0.0.0.0"
	cfg.ListenPort = 5353
	cfg.Metrics.Address = "0.0.0.0"
	cfg.Metrics.Port = 8080
	cfg.DNS.DefaultUpstreamGroup = "default"
	cfg.DNS.UpstreamGroups = map[string]config.UpstreamGroup{"default": forwardGroup()}
	cfg.DNS.Proxies = map[string]config.Proxy{}
	return cfg
}

func forwardGroup() config.UpstreamGroup {
	return config.UpstreamGroup{Type: "forward", Servers: []string{"192.0.2.53:53"}, QueryTimeout: "2s", MaxConcurrentRequests: 8}
}

func dohConfig(endpoint string, bootstrap []string, caFile string) config.Config {
	cfg := baseConfig()
	cfg.DNS.DefaultUpstreamGroup = "trusted"
	cfg.DNS.UpstreamGroups["trusted"] = config.UpstreamGroup{
		Type: "doh", Strategy: "parallel",
		Endpoints:              []config.Endpoint{{URL: endpoint, BootstrapIPs: bootstrap, CAFile: caFile}},
		Egress:                 config.Egress{Mode: "direct"},
		QueryTimeout:           "2s",
		ConnectTimeout:         "1s",
		TLSHandshakeTimeout:    "1s",
		MaxAttemptsPerEndpoint: 1,
		MaxConcurrentRequests:  8,
	}
	return cfg
}

func newRouter(t *testing.T, cfg config.Config) *Router {
	t.Helper()
	metrics := NewMetrics(prometheus.NewRegistry())
	router, err := New(cfg, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func query(name string) *dns.Msg {
	request := new(dns.Msg)
	request.SetQuestion(name, dns.TypeA)
	request.Id = 1245
	return request
}

func startDNSServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		_ = udp.Close()
		t.Fatal(err)
	}
	udpServer := &dns.Server{PacketConn: udp, Handler: handler}
	tcpServer := &dns.Server{Listener: tcp, Handler: handler}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = udpServer.ShutdownContext(ctx)
		_ = tcpServer.ShutdownContext(ctx)
	})
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func startDoHServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "server-ca.pem")
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificate) {
		t.Fatal("test server certificate did not parse")
	}
	return server, caFile
}

func writeDNSAnswer(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Content-Type") != "application/dns-message" {
		http.Error(writer, "bad content type", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "read query", http.StatusBadRequest)
		return
	}
	query := new(dns.Msg)
	if err := query.Unpack(data); err != nil {
		http.Error(writer, "unpack query", http.StatusBadRequest)
		return
	}
	response := new(dns.Msg)
	response.SetReply(query)
	response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("192.0.2.8").To4()}}
	body, err := response.Pack()
	if err != nil {
		http.Error(writer, "pack response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/dns-message")
	_, _ = writer.Write(body)
}
