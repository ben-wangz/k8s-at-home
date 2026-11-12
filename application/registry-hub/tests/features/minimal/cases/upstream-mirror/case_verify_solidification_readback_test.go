package upstreammirror

import (
	"encoding/json"
	"testing"
)

func TestVerifySolidificationReadbackUsesMatchingSolidificationEvent(t *testing.T) {
	lines := []string{
		`{"msg":"solidification blob verified","blob_digest":"sha256:other","distribution_readback_bytes":16}`,
		`{"msg":"solidification blob verified","blob_digest":"sha256:config","distribution_readback_bytes":0}`,
	}
	result, err := verifySolidificationReadback(lines, "sha256:config")
	if err != nil {
		t.Fatal(err)
	}
	if result["solidificationDistributionReadbackBytes"] != 0 {
		t.Fatalf("solidification result = %#v", result)
	}
}

func TestVerifySolidificationReadbackRejectsPayloadRead(t *testing.T) {
	lines := []string{`{"msg":"solidification blob verified","blob_digest":"sha256:config","distribution_readback_bytes":29}`}
	if _, err := verifySolidificationReadback(lines, "sha256:config"); err == nil {
		t.Fatal("verifySolidificationReadback accepted a solidification payload readback")
	}
}

func TestVerifySolidificationReadbackRequiresMatchingEvent(t *testing.T) {
	line, err := json.Marshal(map[string]any{"msg": "solidification blob verified", "blob_digest": "sha256:other", "distribution_readback_bytes": 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySolidificationReadback([]string{string(line)}, "sha256:config"); err == nil {
		t.Fatal("verifySolidificationReadback accepted missing matching event")
	}
}
