package app

import (
	"encoding/pem"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"testing"
)

func descriptorList(descriptor artifact.Descriptor) *[]artifact.Descriptor {
	items := []artifact.Descriptor{descriptor}
	return &items
}

func newTestDistribution(t *testing.T, server *httptest.Server) *distribution.Client {
	t.Helper()
	caPath := filepath.Join(t.TempDir(), "distribution-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: base, DistributionCA: caPath})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
