//go:build integration

// Special-HEAD coverage: detached HEAD sources and empty (unborn HEAD)
// repositories must back up, restore, and report honestly instead of
// fabricating a default branch.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git-repo-backup/internal/manifest"
)

// TestLocalDetachedHeadAndEmptyRepo verifies:
//   - a detached-HEAD origin is mirrored with a detached HEAD and the OID
//     recorded in the manifest;
//   - an empty origin keeps its legal unborn HEAD, archives successfully,
//     and reports refCount 0 without inventing refs.
func TestLocalDetachedHeadAndEmptyRepo(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)

	originDetached := seedDetachedRepo(t, dir, "detached")
	originEmpty := seedEmptyRepo(t, dir, "empty")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	repos := fmt.Sprintf(`repositories:
  - name: detached
    url: ssh://root@127.0.0.1:%d%s
  - name: empty
    url: ssh://root@127.0.0.1:%d%s
`, srv.port, originDetached, srv.port, originEmpty)
	cfg := writeRunConfig(t, dir, backupRoot, workspace, repos, 3)

	code, out := runBackup(t, bin, cfg)
	if code != 0 {
		t.Fatalf("run failed (%d):\n%s", code, out)
	}
	runDir := filepath.Join(backupRoot, "backups", latestBackupID(t, backupRoot))

	// Manifest must record the detached OID and zero refs for the empty repo.
	var m manifest.Manifest
	if err := json.Unmarshal(mustRead(t, filepath.Join(runDir, "manifest.json")), &m); err != nil {
		t.Fatal(err)
	}
	entries := map[string]manifest.RepositoryEntry{}
	for _, e := range m.Repositories {
		entries[e.Name] = e
	}
	wantOID := strings.TrimSpace(runCmd(t, dir, nil, "git", "-C", originDetached, "rev-parse", "HEAD"))
	d := entries["detached"]
	if d.HeadOID != wantOID || d.HeadRef != "" {
		t.Fatalf("detached HEAD not recorded correctly: %+v", d)
	}
	// An empty origin keeps its unborn HEAD: ls-remote does not advertise
	// unborn symrefs, so the mirror keeps whatever the clone produced.
	e := entries["empty"]
	if e.RefCount != 0 || e.HeadOID != "" || !strings.HasPrefix(e.HeadRef, "refs/heads/") {
		t.Fatalf("empty repo must keep an unborn symbolic HEAD: %+v", e)
	}

	// Extract and verify both archives against their origins. The empty
	// repo has no refs; its mirror must be a valid empty bare repository.
	restoreDir := filepath.Join(dir, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"detached", "empty"} {
		runCmd(t, restoreDir, nil, "tar", "-xzf",
			filepath.Join(runDir, "repositories", name+".tar.gz"), "--no-same-owner")
		mirror := filepath.Join(restoreDir, name+".git")
		runCmd(t, mirror, nil, "git", "-C", mirror, "fsck", "--full")
	}
	detMirror := filepath.Join(restoreDir, "detached.git")
	headOut := runCmd(t, detMirror, nil, "git", "-C", detMirror, "rev-parse", "HEAD")
	if strings.TrimSpace(headOut) != wantOID {
		t.Fatalf("mirror HEAD is not the detached OID: %q vs %q", headOut, wantOID)
	}
	// rev-parse --abbrev-ref prints "HEAD" itself when HEAD is detached;
	// symbolic-ref -q would exit 1 here, which is the expected state.
	abbrev := strings.TrimSpace(runCmd(t, detMirror, nil, "git", "-C", detMirror, "rev-parse", "--abbrev-ref", "HEAD"))
	if abbrev != "HEAD" {
		t.Fatalf("mirror HEAD must be detached, got %q", abbrev)
	}
	emptyMirror := filepath.Join(restoreDir, "empty.git")
	refsOut := runCmd(t, emptyMirror, nil, "git", "-C", emptyMirror, "for-each-ref")
	if strings.TrimSpace(refsOut) != "" {
		t.Fatalf("empty mirror must have no refs: %q", refsOut)
	}

	// Restore of the detached mirror into a fresh bare repo keeps the
	// pushed refs (HEAD detachment is server state; refs must match).
	target := filepath.Join(dir, "detached-target.git")
	runCmd(t, dir, nil, "git", "init", "--bare", target)
	runCmd(t, detMirror, nil, "git", "-C", detMirror, "remote", "set-url", "origin", target)
	runCmd(t, detMirror, nil, "git", "-C", detMirror, "push", "--mirror", "origin")
	if !sameStringSlices(refsAndOIDs(t, dir, target), refsAndOIDs(t, dir, originDetached)) {
		t.Fatal("restored detached repo refs differ from source")
	}
}
