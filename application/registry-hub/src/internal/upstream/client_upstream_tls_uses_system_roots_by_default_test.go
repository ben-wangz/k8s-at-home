package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"registry-hub/internal/config"
	"strings"
	"testing"
	"time"
)

func TestUpstreamTLSUsesSystemRootsByDefault(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_SYSTEM_ROOT_TEST") == "1" {
		chainPath := os.Getenv("REGISTRY_HUB_TEST_CERT_CHAIN")
		data, err := os.ReadFile(chainPath)
		if err != nil {
			t.Fatal(err)
		}
		var certificates []*x509.Certificate
		for len(data) > 0 {
			block, rest := pem.Decode(data)
			if block == nil {
				break
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			certificates = append(certificates, certificate)
			data = rest
		}
		if len(certificates) != 2 {
			t.Fatalf("certificate chain length = %d, want leaf and root", len(certificates))
		}
		clientTLS := newClientTLSConfig(nil)
		if err := clientTLS.VerifyConnection(tls.ConnectionState{
			ServerName: "registry.system-root.test", PeerCertificates: certificates,
		}); err != nil {
			t.Fatalf("system root was not used for default upstream TLS: %v", err)
		}
		return
	}

	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Registry Hub test system root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "registry.system-root.test"},
		DNSNames: []string{"registry.system-root.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	rootPath := directory + "/system-root.pem"
	chainPath := directory + "/chain.pem"
	if err := os.WriteFile(rootPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), 0600); err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	if err := os.WriteFile(chainPath, chain, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestUpstreamTLSUsesSystemRootsByDefault$")
	command.Env = []string{
		"REGISTRY_HUB_SYSTEM_ROOT_TEST=1",
		"REGISTRY_HUB_TEST_CERT_CHAIN=" + chainPath,
		"SSL_CERT_FILE=" + rootPath,
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("system root verification subprocess failed: %v\n%s", err, output)
	}
}

func TestUpstreamHTTPRequiresExplicitAuthorityRule(t *testing.T) {
	client := &Client{rules: map[string]config.TransportRule{}}
	requestURL, err := client.initialURL("registry.example", "/v2/team/image/manifests/latest")
	if err != nil {
		t.Fatalf("create default upstream URL: %v", err)
	}
	if requestURL.Scheme != "https" {
		t.Fatalf("scheme without an explicit rule = %q, want https", requestURL.Scheme)
	}
	client.rules["registry.example"] = config.TransportRule{AllowHTTP: true}
	requestURL, err = client.initialURL("registry.example", "/v2/team/image/manifests/latest")
	if err != nil {
		t.Fatalf("create explicitly enabled HTTP URL: %v", err)
	}
	if requestURL.Scheme != "http" {
		t.Fatalf("scheme with explicit HTTP permission = %q, want http", requestURL.Scheme)
	}
}

func TestUpstreamCredentialsAreScopedByAuthorityAndPull(t *testing.T) {
	var firstRegistryAuth, firstTokenAuth, secondAuthorityAuth, thirdAuthorityAuth string
	var tokenScope string
	var firstRegistryRequests int
	client, err := New(config.Config{UpstreamAuth: map[string]config.UpstreamAuth{
		"8.8.8.8": {Type: "basic", Username: "first-user", Password: "first-password"},
		"1.1.1.1": {Type: "basic", Username: "second-user", Password: "second-password"},
		"9.9.9.9": {Type: "bearer", Token: "third-token"},
	}})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	client.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}
		switch {
		case request.URL.Host == "8.8.8.8" && request.URL.Path == "/token":
			tokenScope = request.URL.Query().Get("scope")
			username, password, _ := request.BasicAuth()
			firstTokenAuth = username + ":" + password
			response.Body = io.NopCloser(strings.NewReader(`{"token":"first-access-token"}`))
		case request.URL.Host == "8.8.8.8":
			firstRegistryRequests++
			if firstRegistryRequests == 1 {
				response.StatusCode = http.StatusUnauthorized
				response.Header.Set("WWW-Authenticate", `Bearer realm="https://8.8.8.8/token",service="registry"`)
			} else {
				firstRegistryAuth = request.Header.Get("Authorization")
			}
		case request.URL.Host == "1.1.1.1":
			username, password, _ := request.BasicAuth()
			secondAuthorityAuth = username + ":" + password
		case request.URL.Host == "9.9.9.9":
			thirdAuthorityAuth = request.Header.Get("Authorization")
		}
		return response, nil
	})}

	for _, authority := range []string{"8.8.8.8", "1.1.1.1", "9.9.9.9"} {
		response, err := client.Do(context.Background(), authority, "team/image", http.MethodGet, "/v2/team/image/manifests/latest", nil, nil)
		if err != nil {
			t.Fatalf("request authority %s: %v", authority, err)
		}
		_ = response.Body.Close()
	}
	if firstTokenAuth != "first-user:first-password" || firstRegistryAuth != "Bearer first-access-token" || tokenScope != "repository:team/image:pull" {
		t.Fatalf("first token auth=%q registry auth=%q scope=%q", firstTokenAuth, firstRegistryAuth, tokenScope)
	}
	if secondAuthorityAuth != "second-user:second-password" {
		t.Fatalf("second authority credential = %q", secondAuthorityAuth)
	}
	if thirdAuthorityAuth != "Bearer third-token" {
		t.Fatalf("third authority credential = %q", thirdAuthorityAuth)
	}
}
