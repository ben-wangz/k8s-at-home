package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ben-wangz/clash-tests/internal/environment"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func runCase(ctx context.Context, app, directory string, record *environment.Record, definition testkit.Definition) ([]testkit.Result, error) {
	if definition.Mode == "browser" || definition.Mode == "mock-browser" {
		return runBrowser(ctx, app, directory, definition, record.Mode)
	}
	path := filepath.Join(directory, "cases", definition.ID)
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	switch definition.ID {
	case "default-render":
		return renderChecks(ctx, app, path, definition)
	case "suite-lifecycle":
		output, err := testkit.Command(ctx, filepath.Join(app, "tests"), []string{"GOWORK=off"}, "go", "test", "-count=1", "./internal/runner", "./internal/environment")
		evidence := filepath.Join(path, "runner-tests.txt")
		if writeErr := os.WriteFile(evidence, output, 0600); writeErr != nil {
			return nil, writeErr
		}
		if err != nil {
			return nil, fmt.Errorf("runner checks failed; see %s", evidence)
		}
		results := []testkit.Evidence{{ID: "runner-tests", Path: evidence}}
		if record.Mode == "integration" {
			lifecycle, err := lifecycleCommands(ctx, app, path, record)
			if err != nil {
				return nil, err
			}
			results = append(results, lifecycle...)
		}
		return []testkit.Result{{ID: definition.Checks[0].ID, Status: "passed", Evidence: results}}, nil
	case "suite-independence":
		return independence(ctx, app, path, definition)
	default:
		return nil, fmt.Errorf("unsupported local case %s", definition.ID)
	}
}

func independence(ctx context.Context, app, directory string, definition testkit.Definition) ([]testkit.Result, error) {
	if err := os.MkdirAll(environment.CacheRoot(), 0700); err != nil {
		return nil, err
	}
	checkout, err := os.MkdirTemp(environment.CacheRoot(), "independence-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkout)
	isolated := filepath.Join(checkout, "application/clash")
	if err := copySource(app, isolated); err != nil {
		return nil, err
	}
	tests := filepath.Join(isolated, "tests")
	variables := []string{"GOWORK=off", "CLASH_TEST_APP_ROOT=" + isolated}
	var evidence []testkit.Evidence
	for _, command := range []struct {
		name string
		args []string
	}{
		{"dependencies", []string{"list", "-deps", "./cmd/clash-tests"}},
		{"list", []string{"run", "./cmd/clash-tests", "list"}},
		{"plan", []string{"run", "./cmd/clash-tests", "plan", "--case", "proxy-selection"}},
		{"runner", []string{"test", "./internal/...", "./fixtures/..."}},
	} {
		output, err := testkit.Command(ctx, tests, variables, "go", command.args...)
		if err != nil {
			return nil, fmt.Errorf("isolated Clash %s failed: %w", command.name, err)
		}
		if command.name == "dependencies" && strings.Contains(string(output), "registry"+"-hub") {
			return nil, fmt.Errorf("forbidden cross-application test dependency")
		}
		path := filepath.Join(directory, command.name+".txt")
		if err := os.WriteFile(path, output, 0600); err != nil {
			return nil, err
		}
		evidence = append(evidence, testkit.Evidence{ID: command.name, Path: path})
	}
	return []testkit.Result{{ID: definition.Checks[0].ID, Status: "passed", Evidence: evidence}}, nil
}

func copySource(source, target string) error {
	return filepath.WalkDir(source, func(path string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		if item.IsDir() {
			if map[string]bool{"node_modules": true, "dist": true, "charts": true, "test-results": true, "playwright-report": true}[item.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(target, relative), 0700)
		}
		if item.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source symlinks are not allowed")
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if strings.HasSuffix(path, ".sh") {
			mode = 0700
		}
		return os.WriteFile(filepath.Join(target, relative), content, mode)
	})
}
