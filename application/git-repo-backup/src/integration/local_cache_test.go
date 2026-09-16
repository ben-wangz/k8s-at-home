//go:build integration

// Persistent-cache acceptance: incremental updates after source changes,
// immutability of earlier archives, and rebuild when the source URL of a
// cached name changes.
package integration

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func digestOfFile(t *testing.T, path string) string {
	t.Helper()
	data := mustRead(t, path)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// TestLocalCacheIncremental runs A, mutates the source (new commit and
// tag, deleted branch, flipped default HEAD), runs B, and verifies:
//   - B's archive matches the new refs/OID/HEAD baseline;
//   - A's archive bytes are unchanged;
//   - the cache actually holds a mirror entry (not an ephemeral clone).
func TestLocalCacheIncremental(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")
	cacheRoot := filepath.Join(dir, "cache")

	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfigOpts(t, dir, runConfig{
		backupRoot: backupRoot, workspace: workspace, cacheRoot: cacheRoot,
		reposYAML: repos, maxBackups: 10, retentionOn: true,
	})

	if code, out := runBackup(t, bin, cfg); code != 0 {
		t.Fatalf("run A failed (%d):\n%s", code, out)
	}
	firstID := latestBackupID(t, backupRoot)
	firstArchive := filepath.Join(backupRoot, "backups", firstID, "repositories", "alpha.tar.gz")
	digestA := digestOfFile(t, firstArchive)
	if _, err := os.Stat(filepath.Join(cacheRoot, "mirrors", "alpha.git")); err != nil {
		t.Fatalf("cache entry missing after run A: %v", err)
	}

	// Mutate the source: new commit, new tag, delete feature, flip HEAD.
	work := filepath.Join(dir, "alpha.work")
	runCmd(t, work, gitEnv(), "git", "commit", "--allow-empty", "-m", "second")
	runCmd(t, work, gitEnv(), "git", "tag", "-a", "v2", "-m", "release two")
	runCmd(t, work, gitEnv(), "git", "push", "origin", "main", "v2")
	runCmd(t, work, gitEnv(), "git", "push", "origin", "--delete", "feature")
	runCmd(t, work, gitEnv(), "git", "push", "origin", "main:refs/heads/trunk")
	runCmd(t, dir, nil, "git", "-C", origin, "symbolic-ref", "HEAD", "refs/heads/trunk")

	time.Sleep(1100 * time.Millisecond)
	if code, out := runBackup(t, bin, cfg); code != 0 {
		t.Fatalf("run B failed (%d):\n%s", code, out)
	}
	secondID := latestBackupID(t, backupRoot)
	if secondID == firstID {
		t.Fatal("run B must produce a new backup ID")
	}
	secondRun := filepath.Join(backupRoot, "backups", secondID)
	verifyExtractedRun(t, secondRun, map[string]string{"alpha": origin})

	// Earlier archive immutable.
	if digestOfFile(t, firstArchive) != digestA {
		t.Fatal("archive from run A changed after run B")
	}
}

// TestLocalCacheURLChangeRebuilds keeps the repository name but points it
// at a different origin: the cache entry must be rebuilt from the new
// source without mixing in the old origin's refs.
func TestLocalCacheURLChangeRebuilds(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	originA := seedSourceRepo(t, dir, "alpha")
	originB := seedSourceRepo(t, dir, "beta")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")
	cacheRoot := filepath.Join(dir, "cache")

	url := func(p string) string {
		return fmt.Sprintf("ssh://root@127.0.0.1:%d%s", srv.port, p)
	}
	cfgA := writeRunConfigOpts(t, dir, runConfig{
		backupRoot: backupRoot, workspace: workspace, cacheRoot: cacheRoot,
		reposYAML:  fmt.Sprintf("repositories:\n  - name: same\n    url: %s\n", url(originA)),
		maxBackups: 10, retentionOn: true,
	})
	if code, out := runBackup(t, bin, cfgA); code != 0 {
		t.Fatalf("run A failed (%d):\n%s", code, out)
	}

	time.Sleep(1100 * time.Millisecond)
	cfgB := writeRunConfigOpts(t, dir, runConfig{
		backupRoot: backupRoot, workspace: workspace, cacheRoot: cacheRoot,
		reposYAML:  fmt.Sprintf("repositories:\n  - name: same\n    url: %s\n", url(originB)),
		maxBackups: 10, retentionOn: true,
	})
	if code, out := runBackup(t, bin, cfgB); code != 0 {
		t.Fatalf("run B failed (%d):\n%s", code, out)
	}
	runDir := filepath.Join(backupRoot, "backups", latestBackupID(t, backupRoot))
	// The rebuilt cache must contain exactly originB's refs for name "same".
	restoreDir := verifyExtractedRun(t, runDir, map[string]string{"same": originB})
	mirror := filepath.Join(restoreDir, "same.git")
	remoteURL := runCmd(t, mirror, nil, "git", "-C", mirror, "config", "--get", "remote.origin.url")
	want := "ssh://git@restore.invalid/repository.git"
	if remoteURL != want+"\n" && remoteURL != want {
		t.Fatalf("archived config must use the placeholder URL, got %q", remoteURL)
	}
}
