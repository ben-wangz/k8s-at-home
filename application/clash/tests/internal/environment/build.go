package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func (e *Environment) Images(ctx context.Context, directory string, record *Record) error {
	fixture := os.Getenv("CLASH_TEST_FIXTURE_IMAGE")
	if fixture == "" {
		repository := os.Getenv("CLASH_TEST_IMAGE_REPOSITORY")
		if repository == "" {
			return fmt.Errorf("CLASH_TEST_IMAGE_REPOSITORY is required")
		}
		build := filepath.Join(directory, "build")
		if err := os.MkdirAll(build, 0700); err != nil {
			return err
		}
		if _, err := execute(ctx, filepath.Join(e.App, "tests"), nil, []string{"GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, "go", "build", "-trimpath", "-o", filepath.Join(build, "clash-fixture"), "./fixtures/traffic"); err != nil {
			return err
		}
		containerfile, err := os.ReadFile(filepath.Join(e.App, "tests/fixtures/Containerfile"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(build, "Containerfile"), containerfile, 0600); err != nil {
			return err
		}
		tag := repository + ":test-" + record.ID
		output, err := execute(ctx, build, nil, nil, "podman", "build", "--platform", "linux/amd64", "-t", tag, ".")
		_ = os.WriteFile(filepath.Join(directory, "fixture-build.log"), output, 0600)
		if err != nil {
			return fmt.Errorf("fixture build failed; see fixture-build.log")
		}
		digestPath := filepath.Join(directory, "fixture-digest")
		output, err = execute(ctx, build, nil, nil, "podman", "push", "--digestfile", digestPath, tag)
		_ = os.WriteFile(filepath.Join(directory, "fixture-push.log"), output, 0600)
		if err != nil {
			return fmt.Errorf("fixture push failed; see fixture-push.log")
		}
		digest, err := os.ReadFile(digestPath)
		if err != nil {
			return err
		}
		fixture = repository + "@" + strings.TrimSpace(string(digest))
	}
	record.FixtureImage = fixture
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
	}
	content, err := os.ReadFile(filepath.Join(e.App, "chart/values.yaml"))
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(content, &values); err != nil {
		return err
	}
	core := os.Getenv("CLASH_TEST_MIHOMO_IMAGE")
	if core == "" {
		core = values.Image.Repository + ":" + values.Image.Tag
	}
	if !strings.Contains(core, "@sha256:") {
		if _, err := execute(ctx, e.App, nil, nil, "podman", "pull", "--platform", "linux/amd64", core); err != nil {
			return fmt.Errorf("cannot pull pinned Mihomo image; configure CLASH_TEST_MIHOMO_IMAGE with a reachable mirror: %w", err)
		}
		output, err := execute(ctx, e.App, nil, nil, "podman", "image", "inspect", core, "--format", "{{json .RepoDigests}}")
		if err != nil {
			return err
		}
		var digests []string
		if err := json.Unmarshal(output, &digests); err != nil || len(digests) == 0 {
			return fmt.Errorf("cannot resolve Mihomo image digest")
		}
		core = digests[0]
	}
	record.MihomoImage = core
	return Save(directory, record)
}
