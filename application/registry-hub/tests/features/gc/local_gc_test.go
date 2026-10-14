package gc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalDistributionGCRemovesUnreferencedBlob(t *testing.T) {
	image := os.Getenv("REGISTRY_HUB_TEST_REGISTRY_IMAGE")
	if image == "" {
		t.Skip("REGISTRY_HUB_TEST_REGISTRY_IMAGE is not set")
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	registryRoot := filepath.Join(root, "registry")
	registryData := filepath.Join(registryRoot, "docker", "registry", "v2")
	repositories := filepath.Join(registryData, "repositories")
	blob := []byte("registry-hub-local-gc-orphan")
	digest := fmt.Sprintf("%x", sha256.Sum256(blob))
	blobData := filepath.Join(registryData, "blobs", "sha256", digest[:2], digest, "data")
	for _, directory := range []string{repositories, filepath.Dir(blobData)} {
		if err := os.MkdirAll(directory, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(blobData, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	configuration := "version: 0.1\nlog:\n  level: error\nstorage:\n  filesystem:\n    rootdirectory: /docker/registry\n  delete:\n    enabled: true\n"
	if err := os.WriteFile(filepath.Join(root, "config.yml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	versionCommand := exec.CommandContext(ctx, podman, "run", "--rm", "--network=none", "--entrypoint=registry", image, "--version")
	versionOutput, err := versionCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read Distribution executable version: %v\n%s", err, strings.TrimSpace(string(versionOutput)))
	}
	if !strings.HasSuffix(strings.TrimSpace(string(versionOutput)), "3.1.2") {
		t.Fatalf("Distribution executable version = %q, want 3.1.2", strings.TrimSpace(string(versionOutput)))
	}
	command := exec.CommandContext(ctx, podman, "run", "--rm", "--network=none", "--user=0",
		"--volume", root+":/docker:rw,z",
		"--env", "REGISTRY_STORAGE_DELETE_ENABLED=true",
		"--entrypoint=registry", image, "garbage-collect", "/docker/config.yml")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Distribution GC failed: %v\n%s", err, strings.TrimSpace(string(output)))
	}
	if _, err := os.Stat(blobData); !os.IsNotExist(err) {
		t.Fatalf("unreferenced blob still exists after GC: stat error = %v", err)
	}
}
