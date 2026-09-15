//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestLocalEndToEnd covers acceptance items for the local backend: two
// repositories archived with valid metadata and marker, extracted archives
// passing fsck with refs matching the source, restore via push --mirror,
// and retention keeping only the configured number of backups.
func TestLocalEndToEnd(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)

	// Two healthy origins plus one that does not exist on the server.
	originA := seedSourceRepo(t, dir, "alpha")
	originB := seedSourceRepo(t, dir, "beta")
	url := func(path string) string {
		return fmt.Sprintf("ssh://root@127.0.0.1:%d%s", srv.port, path)
	}
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	reposHealthy := fmt.Sprintf(`repositories:
  - name: alpha
    url: %s
  - name: beta
    url: %s
`, url(originA), url(originB))

	cfg := writeRunConfig(t, dir, backupRoot, workspace, reposHealthy, 2)
	code, out := runBackup(t, bin, cfg)
	if code != 0 {
		t.Fatalf("healthy run failed (%d):\n%s", code, out)
	}
	backupID := latestBackupID(t, backupRoot)
	runDir := filepath.Join(backupRoot, "backups", backupID)

	// Layout, marker, checksums, manifest.
	for _, name := range []string{"manifest.json", "checksums.sha256", "_SUCCESS",
		"repositories/alpha.tar.gz", "repositories/beta.tar.gz"} {
		if _, err := os.Stat(filepath.Join(runDir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if out := runCmd(t, runDir, nil, "sha256sum", "--check", "checksums.sha256"); !strings.Contains(out, "OK") {
		t.Fatalf("checksum verification failed:\n%s", out)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(runDir, "manifest.json"))), "127.0.0.1") {
		t.Fatal("manifest must not contain source URLs")
	}

	// Extract one archive, fsck it, compare refs with the origin.
	restoreDir := filepath.Join(dir, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runCmd(t, restoreDir, nil, "tar", "-xzf",
		filepath.Join(runDir, "repositories", "alpha.tar.gz"), "--no-same-owner")
	mirror := filepath.Join(restoreDir, "alpha.git")
	runCmd(t, mirror, nil, "git", "-C", mirror, "fsck", "--full")
	refsOf := func(repo string) []string {
		out := runCmd(t, dir, nil, "git", "-C", repo, "for-each-ref", "--format=%(refname)")
		return strings.Fields(out)
	}
	got, want := refsOf(mirror), refsOf(originA)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refs mismatch:\ngot  %v\nwant %v", got, want)
	}
	if strings.Contains(strings.Join(got, ","), "refs/heads/temporary") {
		t.Fatal("deleted branch must not be present")
	}
	if strings.Join(got, ",") == "" || !strings.Contains(strings.Join(got, ","), "refs/tags/v1") {
		t.Fatal("annotated tag missing")
	}
	// HEAD must mirror the origin's symbolic HEAD.
	headMirror := strings.TrimSpace(runCmd(t, mirror, nil, "git", "-C", mirror, "symbolic-ref", "HEAD"))
	headOrigin := strings.TrimSpace(runCmd(t, originA, nil, "git", "-C", originA, "symbolic-ref", "HEAD"))
	if headMirror != headOrigin {
		t.Fatalf("HEAD mismatch: %q vs %q", headMirror, headOrigin)
	}

	// Restore by pushing the mirror into a new bare repository.
	target := filepath.Join(dir, "target.git")
	runCmd(t, dir, nil, "git", "init", "--bare", target)
	runCmd(t, mirror, nil, "git", "-C", mirror, "remote", "set-url", "origin", target)
	runCmd(t, mirror, nil, "git", "-C", mirror, "push", "--mirror", "origin")
	got, want = refsOf(target), refsOf(originA)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("restored refs mismatch:\ngot  %v\nwant %v", got, want)
	}

	// Retention: two more runs, maxBackups=2 keeps only the newest two.
	first := backupID
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond) // distinct backup IDs (one per second)
		if code, out = runBackup(t, bin, cfg); code != 0 {
			t.Fatalf("subsequent run failed (%d):\n%s", code, out)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(backupRoot, "backups"))
	if len(entries) != 2 {
		t.Fatalf("retention must keep exactly 2 backups, got %d", len(entries))
	}
	for _, e := range entries {
		if e.Name() == first {
			t.Fatal("oldest backup must have been deleted")
		}
	}

	// All-or-nothing: an unreachable repository fails the run and leaves no
	// new backup and no marker anywhere in staging.
	reposBroken := fmt.Sprintf(`repositories:
  - name: alpha
    url: %s
  - name: missing
    url: ssh://root@127.0.0.1:%d/this/repo/does/not/exist.git
`, url(originA), srv.port)
	cfgBroken := writeRunConfig(t, dir, backupRoot, workspace, reposBroken, 2)
	time.Sleep(1100 * time.Millisecond)
	before := latestBackupID(t, backupRoot)
	code, out = runBackup(t, bin, cfgBroken)
	if code == 0 {
		t.Fatal("unreachable repository must fail the run")
	}
	if after := latestBackupID(t, backupRoot); after != before {
		t.Fatal("failed run must not publish a backup")
	}
	staging, _ := os.ReadDir(filepath.Join(backupRoot, ".staging"))
	for _, e := range staging {
		marker := filepath.Join(backupRoot, ".staging", e.Name(), "_SUCCESS")
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("failed run must not leave a success marker")
		}
	}
}

// TestLocalSecondFailureNoOverwrite verifies a same-ID collision cannot
// overwrite an existing backup: the directory is pre-created and the run
// must fail instead.
func TestLocalSecondFailureNoOverwrite(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfig(t, dir, backupRoot, workspace, repos, 3)

	// Pre-create the backup directories the run is about to claim for the
	// current and next second; whichever it picks, the run must refuse to
	// overwrite instead of publishing.
	now := time.Now().UTC()
	hijacked := []string{
		filepath.Join(backupRoot, "backups", now.Format("20060102T150405Z")),
		filepath.Join(backupRoot, "backups", now.Add(time.Second).Format("20060102T150405Z")),
	}
	for _, dir := range hijacked {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sentinel"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out := runBackup(t, bin, cfg)
	if code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("expected same-second collision failure, got code=%d:\n%s", code, out)
	}
	for _, dir := range hijacked {
		if _, err := os.Stat(filepath.Join(dir, "sentinel")); err != nil {
			t.Fatalf("existing backup directory %s must not be modified", dir)
		}
	}
}
