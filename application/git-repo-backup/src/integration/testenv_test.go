//go:build integration

// Shared helpers for the local-backend integration tests.
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// runBackup invokes the built binary and returns its exit code and output.
func runBackup(t *testing.T, bin, configPath string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, "run", "--config", configPath)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run binary: %v\n%s", err, out)
	}
	return code, string(out)
}

// runConfig controls the rendered test configuration.
type runConfig struct {
	backupRoot   string
	workspace    string
	cacheRoot    string // empty disables the cache
	reposYAML    string
	maxBackups   int
	retentionOn  bool
	minFreeBytes string // empty uses a small default
}

// writeRunConfig renders a complete local-backend configuration.
func writeRunConfig(t *testing.T, dir, backupRoot, workspace string, reposYAML string, maxBackups int) string {
	t.Helper()
	return writeRunConfigOpts(t, dir, runConfig{
		backupRoot: backupRoot, workspace: workspace,
		reposYAML: reposYAML, maxBackups: maxBackups, retentionOn: true,
	})
}

func writeRunConfigOpts(t *testing.T, dir string, rc runConfig) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.yaml")
	reposPath := filepath.Join(dir, "repositories.yaml")
	if err := os.WriteFile(reposPath, []byte(rc.reposYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	minFree := rc.minFreeBytes
	if minFree == "" {
		minFree = "1"
	}
	cacheBlock := "  enabled: false\n  root: /nonexistent-cache\n"
	if rc.cacheRoot != "" {
		cacheBlock = fmt.Sprintf("  enabled: true\n  root: %s\n", rc.cacheRoot)
	}
	body := fmt.Sprintf(`schemaVersion: 1
repositoriesFile: %s
ssh:
  privateKeyFile: /etc/git-repo-backup/ssh/id
  knownHostsFile: /etc/git-repo-backup/ssh/known_hosts
storage:
  type: local
  local:
    root: %s
workspace:
  root: %s
cache:
%sbackup:
  maxRunDuration: 10m
  gitTimeout: 2m
  minFreeBytes: %s
retention:
  enabled: %t
  maxBackups: %d
  maxAge: ""
  incompleteMaxAge: 30m
log:
  level: debug
`, reposPath, rc.backupRoot, rc.workspace, cacheBlock, minFree, rc.retentionOn, rc.maxBackups)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// latestBackupID returns the newest backup directory name.
func latestBackupID(t *testing.T, backupRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(backupRoot, "backups"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no backups: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names[len(names)-1]
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
