package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteValuesRegistryImageDigestOverride(t *testing.T) {
	const registryDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY", "registry.example.test/test/registry")
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_DIGEST", registryDigest)

	content := writeEnvironmentValues(t)
	sections := strings.SplitN(content, "\nregistry:\n", 2)
	if len(sections) != 2 || !strings.Contains(sections[1], "digest: '"+registryDigest+"'") {
		t.Fatalf("registry digest override was not written: %s", content)
	}
}

func TestWriteValuesOmitsUnsetRegistryImageDigest(t *testing.T) {
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY", "registry.example.test/test/registry")
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_DIGEST", "")

	content := writeEnvironmentValues(t)
	sections := strings.SplitN(content, "\nregistry:\n", 2)
	if len(sections) != 2 || strings.Contains(sections[1], "digest:") {
		t.Fatalf("unset registry digest unexpectedly overrode scenario values: %s", content)
	}
}

func TestWriteValuesRejectsInvalidRegistryImageDigest(t *testing.T) {
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY", "registry.example.test/test/registry")
	t.Setenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE_DIGEST", "latest")
	path := filepath.Join(t.TempDir(), "environment-values.yaml")

	if err := (Environment{}).WriteValues(path, testBackendImage()); err == nil {
		t.Fatal("invalid registry digest was accepted")
	}
}

func writeEnvironmentValues(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "environment-values.yaml")
	if err := (Environment{}).WriteValues(path, testBackendImage()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func testBackendImage() ImageBuild {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return ImageBuild{Reference: "backend.example.test/registry-hub/backend@" + digest, Digest: digest}
}
