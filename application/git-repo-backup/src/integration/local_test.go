//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"git-repo-backup/internal/storage"
)

// refsAndOIDs returns sorted "refname objectname" lines: identity is the
// (ref, OID) pair, not just the ref name.
func refsAndOIDs(t *testing.T, dir, repo string) []string {
	t.Helper()
	out := runCmd(t, dir, nil, "git", "-C", repo, "for-each-ref", "--format=%(refname) %(objectname)")
	var lines []string
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	sort.Strings(lines)
	return lines
}

func sameStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// verifyExtractedRun checks one published local run: checksums verify,
// every configured repository extracts, passes fsck, and its refs/OIDs and
// symbolic HEAD equal the frozen origin baseline.
func verifyExtractedRun(t *testing.T, runDir string, origins map[string]string) string {
	t.Helper()
	if out := runCmd(t, runDir, nil, "sha256sum", "--check", "checksums.sha256"); !strings.Contains(out, "OK") {
		t.Fatalf("checksum verification failed:\n%s", out)
	}
	restoreDir := filepath.Join(filepath.Dir(runDir), "restore-"+filepath.Base(runDir))
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, origin := range origins {
		runCmd(t, restoreDir, nil, "tar", "-xzf",
			filepath.Join(runDir, "repositories", name+".tar.gz"), "--no-same-owner")
		mirror := filepath.Join(restoreDir, name+".git")
		runCmd(t, mirror, nil, "git", "-C", mirror, "fsck", "--full")
		if !sameStringSlices(refsAndOIDs(t, restoreDir, mirror), refsAndOIDs(t, restoreDir, origin)) {
			t.Fatalf("refs/OID mismatch for %s", name)
		}
		headMirror := strings.TrimSpace(runCmd(t, mirror, nil, "git", "-C", mirror, "symbolic-ref", "HEAD"))
		headOrigin := strings.TrimSpace(runCmd(t, restoreDir, nil, "git", "-C", origin, "symbolic-ref", "HEAD"))
		if headMirror != headOrigin {
			t.Fatalf("HEAD mismatch for %s: %q vs %q", name, headMirror, headOrigin)
		}
	}
	return restoreDir
}

// assertNoSecretLeak scans captured output, metadata, and every extracted
// file for the test key marker.
func assertNoSecretLeak(t *testing.T, output, runDir, restoreDir string) {
	t.Helper()
	manifest := string(mustRead(t, filepath.Join(runDir, "manifest.json")))
	checksums := string(mustRead(t, filepath.Join(runDir, "checksums.sha256")))
	for name, haystack := range map[string]string{
		"run output": output,
		"manifest":   manifest,
		"checksums":  checksums,
	} {
		if strings.Contains(haystack, secretMarker) {
			t.Fatalf("secret marker leaked into %s", name)
		}
	}
	if strings.Contains(manifest, "127.0.0.1") {
		t.Fatal("manifest must not contain source URLs")
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			data, err := os.ReadFile(p)
			if err == nil && bytes.Contains(data, []byte(secretMarker)) {
				return fmt.Errorf("secret marker in %s", p)
			}
		}
		return nil
	}
	if err := walk(restoreDir); err != nil {
		t.Fatalf("archive leak: %v", err)
	}
}

// TestLocalEndToEnd covers local-backend acceptance: two repositories
// archived with valid metadata and marker, checksums, fsck, refs/OID/HEAD
// comparison against the frozen source for every repository, restore via
// push --mirror into a fresh bare repo, secret-leak scan, retention, and
// all-or-nothing failure.
func TestLocalEndToEnd(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)

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
	for _, name := range []string{"manifest.json", "checksums.sha256", "_SUCCESS",
		"repositories/alpha.tar.gz", "repositories/beta.tar.gz"} {
		if _, err := os.Stat(filepath.Join(runDir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	restoreDir := verifyExtractedRun(t, runDir, map[string]string{"alpha": originA, "beta": originB})
	assertNoSecretLeak(t, out, runDir, restoreDir)

	// Restore by pushing one mirror into a fresh bare repository and
	// comparing the complete ref/OID sets.
	restoreMirror := filepath.Join(restoreDir, "alpha.git")
	target := filepath.Join(dir, "target.git")
	runCmd(t, dir, nil, "git", "init", "--bare", target)
	runCmd(t, restoreMirror, nil, "git", "-C", restoreMirror, "remote", "set-url", "origin", target)
	runCmd(t, restoreMirror, nil, "git", "-C", restoreMirror, "push", "--mirror", "origin")
	if !sameStringSlices(refsAndOIDs(t, dir, target), refsAndOIDs(t, dir, originA)) {
		t.Fatal("restored refs/OIDs differ from source")
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
	ids := 0
	for _, e := range entries {
		if _, err := storage.ParseBackupID(e.Name()); err != nil {
			continue // restore/unrelated directories are not backups
		}
		ids++
		if e.Name() == first {
			t.Fatal("oldest backup must have been deleted")
		}
	}
	if ids != 2 {
		t.Fatalf("retention must keep exactly 2 backups, got %d", ids)
	}

	// All-or-nothing: an unreachable repository fails the run, publishes
	// nothing, and leaves no marker anywhere in staging.
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
		if _, err := os.Stat(filepath.Join(backupRoot, ".staging", e.Name(), "_SUCCESS")); err == nil {
			t.Fatal("failed run must not leave a success marker")
		}
	}
}
