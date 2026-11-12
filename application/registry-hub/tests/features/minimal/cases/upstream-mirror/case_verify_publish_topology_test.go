package upstreammirror

import (
	"fmt"
	"strings"
	"testing"
)

func TestVerifyPublishTopologyAcceptsWriteTimeIntegrityTrace(t *testing.T) {
	lines := writeTimeIntegrityTrace()
	lines = append(lines[:5], append([]string{
		registryAccessLine("GET", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/sha256:child", 200),
	}, lines[5:]...)...)
	result, err := verifyPublishTopology(lines, "sha256:child", "sha256:index", "sha256:config")
	if err != nil {
		t.Fatal(err)
	}
	if result["publishTopologyOrdered"] != true {
		t.Fatalf("publish topology result = %#v", result)
	}
}

func TestVerifyPublishTopologyRejectsSuccessfulBlobPayloadReadback(t *testing.T) {
	lines := writeTimeIntegrityTrace()
	lines = append(lines[:3], append([]string{
		registryAccessLine("GET", "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/sha256:config", 200),
	}, lines[3:]...)...)
	if _, err := verifyPublishTopology(lines, "sha256:child", "sha256:index", "sha256:config"); err == nil || !strings.Contains(err.Error(), "blob payload GET readback") {
		t.Fatalf("verifyPublishTopology error = %v, want blob payload readback rejection", err)
	}
}

func writeTimeIntegrityTrace() []string {
	return []string{
		registryAccessLine("HEAD", "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/sha256:config", 404),
		registryAccessLine("PUT", "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/uploads/session?digest=sha256%3Aconfig", 201),
		registryAccessLine("HEAD", "/v2/egithub_2ecom/registry-hub-test/fixture/blobs/sha256:config", 200),
		registryAccessLine("GET", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/sha256:child", 404),
		registryAccessLine("PUT", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/sha256:child", 201),
		registryAccessLine("GET", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/sha256:index", 404),
		registryAccessLine("PUT", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/sha256:index", 201),
		registryAccessLine("HEAD", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/minimal-discovery", 404),
		registryAccessLine("PUT", "/v2/egithub_2ecom/registry-hub-test/fixture/manifests/minimal-discovery", 201),
	}
}

func registryAccessLine(method, uri string, status int) string {
	return fmt.Sprintf("http.request.method=%s http.request.uri=%s http.response.status=%d", method, uri, status)
}
