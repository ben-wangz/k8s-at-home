//go:build integration

// Repository seeders for integration tests: custom advertised refs,
// incompressible payloads for multipart coverage, detached HEAD, and empty
// origins.
package integration

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// secretMarker is embedded as the SSH client key comment; the leak-scan
// asserts it never appears in logs, manifests, checksums, or archives.
const secretMarker = "IT-SECRET-MARKER-7f3a91c2"

// seedDetachedRepo creates a bare origin whose HEAD is detached at the main
// commit, exercising the detached-HEAD backup and restore path.
func seedDetachedRepo(t *testing.T, dir, name string) string {
	t.Helper()
	bare := seedSourceRepo(t, dir, name)
	oid := strings.TrimSpace(runCmd(t, dir, nil, "git", "-C", bare, "rev-parse", "refs/heads/main"))
	runCmd(t, dir, nil, "git", "-C", bare, "update-ref", "--no-deref", "HEAD", oid)
	return bare
}

// seedEmptyRepo creates a bare origin with no commits (unborn HEAD).
func seedEmptyRepo(t *testing.T, dir, name string) string {
	t.Helper()
	bare := filepath.Join(dir, name+".git")
	runCmd(t, dir, nil, "git", "init", "--bare", "--initial-branch=main", bare)
	return bare
}

// seedIncompressibleRepo creates a bare origin holding sizeMB of random
// data, which stays above the multipart threshold after gzip.
func seedIncompressibleRepo(t *testing.T, dir, name string, sizeMB int) string {
	t.Helper()
	work := filepath.Join(dir, name+".work")
	bare := filepath.Join(dir, name+".git")
	env := gitEnv()
	runCmd(t, dir, nil, "git", "init", "--bare", bare)
	runCmd(t, dir, nil, "git", "init", "--initial-branch=main", work)
	payload := filepath.Join(work, "payload.bin")
	f, err := os.Create(payload)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for written := 0; written < sizeMB; {
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		if n, err := f.Write(buf); err != nil {
			t.Fatal(err)
		} else {
			written += n / (1 << 20)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runCmd(t, work, env, "git", "add", "payload.bin")
	runCmd(t, work, env, "git", "commit", "-m", "random payload")
	runCmd(t, work, env, "git", "remote", "add", "origin", bare)
	runCmd(t, work, env, "git", "push", "origin", "main")
	return bare
}

// gitPushFixture pushes a set of explicit refs into a bare origin; used to
// exercise custom advertised refs.
func pushCustomRef(t *testing.T, dir, bare, localRef, remoteRef string) {
	t.Helper()
	work := filepath.Join(dir, "custom-ref-work")
	if _, err := os.Stat(work); err != nil {
		runCmd(t, dir, nil, "git", "init", "--initial-branch=main", work)
		runCmd(t, work, gitEnv(), "git", "commit", "--allow-empty", "-m", "custom ref base")
	}
	cmd := exec.Command("git", "-C", work, "push", bare, fmt.Sprintf("%s:%s", localRef, remoteRef))
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("push custom ref: %v\n%s", err, out)
	}
}
