package dnsrouter

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
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
	"split-dns/internal/config"
)

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
