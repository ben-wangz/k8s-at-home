package environment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const backendRepository = "container-registry.internal.home.geekcity.tech:32443/registry-hub/backend"

type ImageBuild struct {
	Reference string
	Digest    string
	Tag       string
}

type SourceIdentity struct {
	SHA256   string
	Revision string
	Status   string
}

func (environment Environment) Source(ctx context.Context) (SourceIdentity, error) {
	files := []string{
		filepath.Join(environment.AppDir, "src"),
		filepath.Join(environment.AppDir, "container"),
		filepath.Join(environment.AppDir, "chart"),
		filepath.Join(environment.AppDir, "tests"),
	}
	hash := sha256.New()
	for _, path := range files {
		if err := hashPath(hash, environment.Root, path); err != nil {
			return SourceIdentity{}, err
		}
	}
	result, err := testkit.Command(ctx, "git", "-C", environment.Root, "rev-parse", "HEAD")
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("read Git revision: %w", err)
	}
	revision := strings.TrimSpace(string(result.Stdout))
	statusResult, err := testkit.Command(ctx, "git", "-C", environment.Root, "status", "--short", "--", "application/registry-hub/src", "application/registry-hub/container", "application/registry-hub/chart", "application/registry-hub/tests")
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("read Git worktree status: %w", err)
	}
	status := strings.TrimSpace(string(statusResult.Stdout))
	if status == "" {
		status = "clean"
	}
	return SourceIdentity{SHA256: hex.EncodeToString(hash.Sum(nil)), Revision: revision, Status: status}, nil
}

func hashPath(destination io.Writer, root, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return hashFile(destination, root, path)
	}
	var paths []string
	err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, current)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, current := range paths {
		if err := hashFile(destination, root, current); err != nil {
			return err
		}
	}
	return nil
}

func hashFile(destination io.Writer, root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(destination, "%s\x00", filepath.ToSlash(relative)); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(destination, file)
	return err
}

func (environment Environment) BuildAndPush(ctx context.Context, id, stateDir string) (ImageBuild, error) {
	tag := "test-" + id
	taggedReference := backendRepository + ":" + tag
	goImage := environmentValue("REGISTRY_HUB_TEST_GO_IMAGE", "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/golang:1.26-alpine")
	runtimeImage := environmentValue("REGISTRY_HUB_TEST_RUNTIME_IMAGE", "container-registry-mirror.internal.home.geekcity.tech:32443/gcr.io/distroless/static-debian12:nonroot")
	goProxy := environmentValue("REGISTRY_HUB_TEST_GOPROXY", "https://goproxy.cn,direct")
	platform := environmentValue("REGISTRY_HUB_TEST_PLATFORM", "linux/amd64")
	arguments := []string{
		"build", "--platform", platform,
		"--build-arg", "GO_IMAGE=" + goImage,
		"--build-arg", "RUNTIME_IMAGE=" + runtimeImage,
		"--build-arg", "GOPROXY=" + goProxy,
		"--file", filepath.Join(environment.AppDir, "container", "Containerfile"),
		"--tag", taggedReference, environment.Root,
	}
	if _, err := testkit.Command(ctx, "podman", arguments...); err != nil {
		return ImageBuild{}, err
	}
	digestPath := filepath.Join(stateDir, "pushed-image-digest")
	if err := os.Remove(digestPath); err != nil && !os.IsNotExist(err) {
		return ImageBuild{}, err
	}
	if _, err := testkit.Command(ctx, "podman", "push", "--digestfile", digestPath, taggedReference); err != nil {
		return ImageBuild{}, err
	}
	encodedDigest, err := os.ReadFile(digestPath)
	if err != nil {
		return ImageBuild{}, fmt.Errorf("read pushed backend digest: %w", err)
	}
	digest := strings.TrimSpace(string(encodedDigest))
	if !validDigest(digest) {
		return ImageBuild{}, fmt.Errorf("podman returned an invalid pushed image digest")
	}
	if err := os.Chmod(digestPath, 0600); err != nil {
		return ImageBuild{}, err
	}
	return ImageBuild{Reference: backendRepository + "@" + digest, Digest: digest, Tag: tag}, nil
}

func validDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	return err == nil
}

func (environment Environment) WriteValues(path string, image ImageBuild) error {
	repository, _, found := strings.Cut(image.Reference, "@sha256:")
	if !found || repository == "" || strings.ContainsAny(repository, "'\n\r") {
		return fmt.Errorf("backend image reference is invalid")
	}
	registryRepository := environmentValue("REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY", "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/registry")
	if strings.ContainsAny(registryRepository, "'\n\r") {
		return fmt.Errorf("registry image repository contains invalid YAML characters")
	}
	content := fmt.Sprintf("backend:\n  image:\n    repository: '%s'\n    digest: '%s'\nregistry:\n  image:\n    repository: '%s'\n", repository, image.Digest, registryRepository)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("write environment values: %w", err)
	}
	return nil
}

func environmentValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (environment Environment) UseImageReference(reference string) (ImageBuild, error) {
	repository, digest, found := strings.Cut(reference, "@sha256:")
	if !found || repository == "" || len(digest) != 64 {
		return ImageBuild{}, fmt.Errorf("backend image must be pinned by repository@sha256 digest")
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.TrimSpace(repository) != repository {
		return ImageBuild{}, fmt.Errorf("backend image digest is invalid")
	}
	fullDigest := "sha256:" + digest
	return ImageBuild{Reference: repository + "@" + fullDigest, Digest: fullDigest}, nil
}
