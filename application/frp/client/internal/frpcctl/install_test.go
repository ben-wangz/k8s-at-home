package frpcctl

import (
	"strings"
	"testing"
)

func TestValidateFRPCConfig(t *testing.T) {
	t.Parallel()
	valid := []byte(`
auth.method = "token"
auth.tokenSource.type = "file"
auth.tokenSource.file.path = "/run/secrets/frp-auth"
`)
	if err := validateFRPCConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	invalid := append(valid, []byte("\nauth.token = \"plaintext\"\n")...)
	if err := validateFRPCConfig(invalid); err == nil {
		t.Fatal("config with a credential literal was accepted")
	}
}

func TestRenderQuadletEnablesBootRecovery(t *testing.T) {
	t.Parallel()
	quadlet := renderQuadlet("docker.io/fatedier/frpc:v0.70.1", "frp-auth")
	for _, expected := range []string{
		"Network=host",
		"Secret=frp-auth,type=mount,target=frp-auth",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(quadlet, expected) {
			t.Errorf("quadlet does not contain %q", expected)
		}
	}
}
