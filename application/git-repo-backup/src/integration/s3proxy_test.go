//go:build integration

// A recording TLS reverse proxy in front of the S3 fixture. The backup
// binary is pointed at the proxy (trusting only the proxy CA), so every
// request the program makes is observable: method, key, query, conditional
// headers, and ordering. The proxy preserves the client's Host header so
// SigV4 signatures stay valid at the upstream, and can inject faults:
// dropping the _SUCCESS response after it was applied, or delaying part
// uploads for signal-timing tests.
package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordedRequest is one forwarded S3 request.
type recordedRequest struct {
	Seq         int
	Method      string
	Key         string // object key, path-style bucket prefix stripped
	Query       string // raw query string
	IfNoneMatch bool   // If-None-Match: * present
	Status      int    // upstream status, 0 on transport error
}

type recordingProxy struct {
	caFile   string
	cert     string
	key      string
	upstream *url.URL
	bucket   string
	base     *http.Transport
	server   *http.Server

	mu       sync.Mutex
	seq      int
	requests []recordedRequest

	dropMarkerResponse atomic.Bool
	delayPartUploads   atomic.Int64 // nanoseconds; 0 disables
}

// startRecordingProxy brings up the proxy on an ephemeral port and returns
// its endpoint URL (https://127.0.0.1:<port>) and CA file for the program
// under test.
func startRecordingProxy(t *testing.T, f s3FixtureInfo) (*recordingProxy, string) {
	t.Helper()
	dir := t.TempDir()
	caFile, certFile, keyFile, err := generateTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixturePool := x509.NewCertPool()
	pemCA, err := os.ReadFile(f.caPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fixturePool.AppendCertsFromPEM(pemCA) {
		t.Fatal("fixture CA unreadable")
	}
	upstream, err := url.Parse(f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	p := &recordingProxy{
		caFile:   caFile,
		cert:     certFile,
		key:      keyFile,
		upstream: upstream,
		bucket:   f.bucket,
		base: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: fixturePool},
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go func() { _ = p.server.ServeTLS(ln, certFile, keyFile) }()
	t.Cleanup(func() { _ = p.server.Close() })
	return p, "https://" + ln.Addr().String()
}

func (p *recordingProxy) handle(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.URL = &url.URL{
		Scheme:   p.upstream.Scheme,
		Host:     p.upstream.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	// Preserve the signed Host so upstream SigV4 verification passes.
	out.Host = r.Host
	out.RequestURI = ""
	resp, err := p.forward(out)
	if err != nil {
		// Abort the client connection instead of synthesizing a status:
		// the program must exercise its response-uncertain paths.
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	for h, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// forward performs and records one upstream request.
func (p *recordingProxy) forward(req *http.Request) (*http.Response, error) {
	if d := time.Duration(p.delayPartUploads.Load()); d > 0 && req.Method == http.MethodPut &&
		strings.Contains(req.URL.RawQuery, "uploadId=") {
		time.Sleep(d)
	}
	resp, err := p.base.RoundTrip(req)
	key := strings.TrimPrefix(req.URL.Path, "/"+p.bucket)
	key = strings.TrimPrefix(key, "/")
	p.record(recordedRequest{
		Method:      req.Method,
		Key:         key,
		Query:       req.URL.RawQuery,
		IfNoneMatch: req.Header.Get("If-None-Match") == "*",
		Status:      respStatus(resp, err),
	})
	if err == nil && p.dropMarkerResponse.Load() && req.Method == http.MethodPut &&
		strings.HasSuffix(key, "/_SUCCESS") {
		// The object was created; hide the response from the client.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("simulated response loss")}
	}
	return resp, err
}

func respStatus(resp *http.Response, err error) int {
	if err != nil || resp == nil {
		return 0
	}
	return resp.StatusCode
}

func (p *recordingProxy) record(r recordedRequest) {
	p.mu.Lock()
	p.seq++
	r.Seq = p.seq
	p.requests = append(p.requests, r)
	p.mu.Unlock()
}

// snapshot returns a copy of the recorded requests.
func (p *recordingProxy) snapshot() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

// generateTLS creates a CA and a localhost/127.0.0.1 server certificate,
// returning caFile, certFile, keyFile.
func generateTLS(dir string) (string, string, string, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "git-repo-backup-it-proxy-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", "", "", err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}
	writePEM := func(name string, typ string, der []byte) (string, error) {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			return "", err
		}
		if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
			f.Close()
			return "", err
		}
		return path, f.Close()
	}
	caFile, err := writePEM("ca.crt", "CERTIFICATE", caDER)
	if err != nil {
		return "", "", "", err
	}
	certFile, err := writePEM("server.crt", "CERTIFICATE", serverDER)
	if err != nil {
		return "", "", "", err
	}
	keyFile, err := writePEM("server.key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey))
	if err != nil {
		return "", "", "", err
	}
	return caFile, certFile, keyFile, nil
}
