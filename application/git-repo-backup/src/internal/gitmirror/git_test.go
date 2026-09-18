package gitmirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRunner builds a Runner with the system git binary; tests use local
// repository paths which the runner itself accepts (URL narrowing happens
// at configuration validation, not in the runner). The protocol allowlist
// is relaxed to permit local-path transports; production only talks SSH or
// HTTPS.
func newRunner(t *testing.T) *Runner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := filepath.Join(t.TempDir(), "home")
	r, err := NewRunner("/usr/bin/git", home, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r.hardening = append([]string{}, hooksFlags...)
	return r
}

func TestBuildSSHCommandPolicies(t *testing.T) {
	tests := []struct {
		name       string
		policy     string
		knownHosts string
		strict     string
		known      string
	}{
		{name: "default accept new", policy: "", knownHosts: "/state/known_hosts", strict: "accept-new", known: "/state/known_hosts"},
		{name: "pinned", policy: "pinned", knownHosts: "/pinned/known_hosts", strict: "yes", known: "/pinned/known_hosts"},
		{name: "none", policy: "none", knownHosts: "/ignored/known_hosts", strict: "no", known: "/dev/null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command, err := BuildSSHCommand(SSHOptions{
				PrivateKeyFile: "/key with space/id",
				KnownHostsFile: tc.knownHosts,
				HostKeyPolicy:  tc.policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(command, "StrictHostKeyChecking="+tc.strict) {
				t.Fatalf("missing strict policy: %s", command)
			}
			if !strings.Contains(command, "UserKnownHostsFile='"+tc.known+"'") {
				t.Fatalf("wrong known_hosts path: %s", command)
			}
			if !strings.Contains(command, "-i '/key with space/id'") {
				t.Fatalf("private key path was not shell quoted: %s", command)
			}
		})
	}
	if _, err := BuildSSHCommand(SSHOptions{HostKeyPolicy: "invalid"}); err == nil {
		t.Fatal("unknown host key policy must be rejected")
	}
}

// seedBareRepo creates a bare repo with one commit on main plus a tag.
func seedBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	bare := filepath.Join(dir, "origin.git")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := exec.Command("git", "init", "--initial-branch=main", work).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "one")
	run("tag", "-a", "v1", "-m", "tag one")
	if err := exec.Command("git", "clone", "--bare", work, bare).Run(); err != nil {
		t.Fatal(err)
	}
	return bare
}

func TestCloneSyncHeadAndFsck(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	bare := seedBareRepo(t)

	mirror := filepath.Join(t.TempDir(), "m.git")
	if err := r.Clone(ctx, bare, mirror); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncHead(ctx, mirror, bare, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Fsck(ctx, mirror); err != nil {
		t.Fatal(err)
	}
	refs, err := r.CountRefs(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if refs != 2 { // refs/heads/main + refs/tags/v1
		t.Fatalf("expected 2 refs, got %d", refs)
	}
	head, err := r.LocalHead(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if !head.Symbolic || head.Ref != "refs/heads/main" || head.OID == "" {
		t.Fatalf("unexpected HEAD state: %+v", head)
	}
}

func TestSyncHeadFollowsDefaultBranchChange(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	bare := seedBareRepo(t)

	mirror := filepath.Join(t.TempDir(), "m.git")
	if err := r.Clone(ctx, bare, mirror); err != nil {
		t.Fatal(err)
	}
	// Flip the origin's HEAD to a new default branch and verify the mirror
	// follows after one refetch.
	clone := filepath.Join(t.TempDir(), "w2")
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cmd := exec.Command("git", "-C", clone, "push", "origin", "main:refs/heads/trunk")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bare, "symbolic-ref", "HEAD", "refs/heads/trunk").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := r.SyncHead(ctx, mirror, bare, true); err != nil {
		t.Fatal(err)
	}
	head, err := r.LocalHead(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if !head.Symbolic || head.Ref != "refs/heads/trunk" {
		t.Fatalf("HEAD did not follow default branch: %+v", head)
	}
}

func TestRemoteUpdatePrunes(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	bare := seedBareRepo(t)

	cacheMirror := filepath.Join(t.TempDir(), "m.git")
	if err := r.Clone(ctx, bare, cacheMirror); err != nil {
		t.Fatal(err)
	}
	if err := r.NormalizeConfig(ctx, cacheMirror, bare); err != nil {
		t.Fatal(err)
	}
	// Push a branch, update, then delete it on the origin and update again.
	clone := filepath.Join(t.TempDir(), "w")
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, args := range [][]string{
		{"-C", clone, "branch", "tmp"},
		{"-C", clone, "push", "origin", "tmp"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := r.RemoteUpdate(ctx, cacheMirror, bare); err != nil {
		t.Fatal(err)
	}
	if refs, _ := r.CountRefs(ctx, cacheMirror); refs != 3 {
		t.Fatalf("expected 3 refs after push, got %d", refs)
	}
	if out, err := exec.Command("git", "-C", clone, "push", "origin", "--delete", "tmp").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := r.RemoteUpdate(ctx, cacheMirror, bare); err != nil {
		t.Fatal(err)
	}
	if refs, _ := r.CountRefs(ctx, cacheMirror); refs != 2 {
		t.Fatalf("pruned refs must disappear, got %d", refs)
	}
}

func TestValidateStructureRejectsAlternates(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	bare := seedBareRepo(t)
	if err := r.ValidateStructure(ctx, bare); err != nil {
		t.Fatalf("clean mirror must validate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bare, "objects", "info", "alternates"), []byte("/elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateStructure(ctx, bare); err == nil {
		t.Fatal("alternates must force a rebuild")
	}
}
