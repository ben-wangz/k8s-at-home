package gc

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestBoundedPodmanDebugOutputRedactsCredentials(t *testing.T) {
	credential := &testkit.BasicCredential{Username: "pull-user", Password: "pull-secret"}
	encoded := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password))
	output := "user=pull-user password=pull-secret auth=" + encoded

	got := boundedPodmanDebugOutput(output, credential)
	for _, secret := range []string{credential.Username, credential.Password, encoded} {
		if strings.Contains(got, secret) {
			t.Fatalf("debug output contains credential material %q", secret)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatal("debug output did not mark redacted credential material")
	}
}

func TestBoundedPodmanDebugOutputKeepsTailWithinLimit(t *testing.T) {
	output := strings.Repeat("x", podmanDebugOutputLimit+100) + "request failed"
	got := boundedPodmanDebugOutput(output, nil)
	if len(got) > podmanDebugOutputLimit {
		t.Fatalf("debug output is %d bytes, limit is %d", len(got), podmanDebugOutputLimit)
	}
	if !strings.HasSuffix(got, "request failed") {
		t.Fatal("debug output did not preserve the final request failure")
	}
}
