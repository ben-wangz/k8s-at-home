package upstreammirror

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
)

func TestFixtureTLSCertificateCoversControlledAuthorities(t *testing.T) {
	certificatePEM, _, err := fixtureTLSMaterial()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("fixture certificate is not PEM encoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, authority := range []string{"github.com", "quay.io"} {
		if err := certificate.VerifyHostname(authority); err != nil {
			t.Errorf("fixture certificate does not cover %s: %v", authority, err)
		}
	}
}

func TestParseRegistryAccessEventWithUploadDigest(t *testing.T) {
	line := `time="2026-10-13T08:25:07.055772052Z" level=info msg="response completed" http.request.method=PUT http.request.uri="/v2/egithub_2ecom/registry-hub-test/fixture/blobs/uploads/01a1345d-8616-7067-83c0-7e82d2f4aac4?_state=state%3Dvalue&digest=sha256%3A44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" http.response.status=201`
	event, ok := parseRegistryAccessEvent(line)
	if !ok {
		t.Fatal("parseRegistryAccessEvent rejected a valid Distribution access log")
	}
	if event.method != "PUT" || event.path != "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/uploads/01a1345d-8616-7067-83c0-7e82d2f4aac4" || event.status != 201 {
		t.Fatalf("unexpected parsed upload event: %#v", event)
	}
	if event.query.Get("digest") != "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Fatalf("unexpected upload digest query: %q", event.query.Get("digest"))
	}
	if event.method != http.MethodPut || event.status != http.StatusCreated || !strings.HasPrefix(event.path, "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/uploads/") {
		t.Fatalf("parsed upload event does not match the solidification request: %#v", event)
	}
}
