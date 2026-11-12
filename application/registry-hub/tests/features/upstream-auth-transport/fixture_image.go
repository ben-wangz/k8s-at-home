package upstreamauthtransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const proxyRepository = "container-registry.internal.home.geekcity.tech:32443/registry-hub/test-upstream-fixture"

func proxyImageTag(sources map[string]string, containerfile []byte, platform string) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", proxyImage, platform, containerfile)
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(hash, "%s\x00%s\x00", name, sources[name])
	}
	return fmt.Sprintf("%s:source-%x", proxyRepository, hash.Sum(nil))
}

func buildProxyImage(ctx context.Context, environment testkit.CaseContext, sources map[string]string) (string, error) {
	containerfile, err := assets.ReadFile("fixture-image/Containerfile")
	if err != nil {
		return "", err
	}
	platform := strings.TrimSpace(os.Getenv("REGISTRY_HUB_TEST_PLATFORM"))
	if platform == "" {
		platform = "linux/amd64"
	}
	tag := proxyImageTag(sources, containerfile, platform)
	directory, err := os.MkdirTemp(environment.StateDir, "fixture-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "Containerfile"), containerfile, 0600); err != nil {
		return "", err
	}
	err = testkit.RunPhase(environment, "fixture-image", func() error {
		if _, err := testkit.Command(ctx, "podman", "build", "--platform", platform, "--build-arg", "GO_IMAGE="+proxyImage, "--tag", tag, directory); err != nil {
			return err
		}
		_, err := testkit.Command(ctx, "podman", "push", "--digestfile", filepath.Join(directory, "digest"), tag)
		return err
	})
	if err != nil {
		return "", err
	}
	digest, err := os.ReadFile(filepath.Join(directory, "digest"))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(digest))
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("invalid fixture image digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		return "", fmt.Errorf("invalid fixture image digest: %w", err)
	}
	return proxyRepository + "@" + value, nil
}
