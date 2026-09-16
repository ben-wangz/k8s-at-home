//go:build integration

// Package integration contains opt-in end-to-end tests against real
// Git/OpenSSH (and optionally a TLS S3 fixture). They are excluded from
// `go test ./...` via the integration build tag.
//
// The local run test needs root: the backup binary pins its SSH material to
// /etc/git-repo-backup/ssh by design, so the fixture must install generated
// keys there. Tests skip when prerequisites are missing.
package integration

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sshdBinary = "/usr/sbin/sshd"

// canRunRooted skips when the fixed /etc/git-repo-backup tree cannot be
// provisioned (non-root developer shell) or tools are missing.
func canRunRooted(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("integration test requires root to provision /etc/git-repo-backup/ssh")
	}
	for _, bin := range []string{sshdBinary, "/usr/bin/ssh-keygen", "/usr/bin/git"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

func runCmd(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=integration", "GIT_AUTHOR_EMAIL=integration@test",
		"GIT_COMMITTER_NAME=integration", "GIT_COMMITTER_EMAIL=integration@test")
}

// seedSourceRepo creates a bare origin with main+feature branches, an
// annotated tag, notes, a custom advertised ref, and a deleted branch,
// returning its path.
func seedSourceRepo(t *testing.T, dir, name string) string {
	t.Helper()
	work := filepath.Join(dir, name+".work")
	bare := filepath.Join(dir, name+".git")
	env := gitEnv()
	runCmd(t, dir, nil, "git", "init", "--bare", bare)
	runCmd(t, dir, nil, "git", "init", "--initial-branch=main", work)
	runCmd(t, work, env, "git", "commit", "--allow-empty", "-m", "initial")
	runCmd(t, work, env, "git", "branch", "feature")
	runCmd(t, work, env, "git", "tag", "-a", "v1", "-m", "release one")
	runCmd(t, work, env, "git", "notes", "add", "-m", "a note")
	runCmd(t, work, env, "git", "update-ref", "refs/custom/it-ref", "HEAD")
	runCmd(t, work, env, "git", "remote", "add", "origin", bare)
	runCmd(t, work, env, "git", "push", "origin", "main", "feature", "v1",
		"refs/notes/commits", "refs/custom/it-ref")
	// A branch that exists only transiently: create, push, delete again.
	runCmd(t, work, env, "git", "branch", "temporary")
	runCmd(t, work, env, "git", "push", "origin", "temporary")
	runCmd(t, work, env, "git", "push", "origin", "--delete", "temporary")
	return bare
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// sshGitServer is a throwaway sshd serving git repositories over SSH.
type sshGitServer struct {
	port    int
	rootDir string
	process *exec.Cmd
}

// startSSHGitServer launches an isolated sshd on 127.0.0.1 that only
// accepts the generated client key restricted to git-shell, and installs
// the pinned known_hosts plus private key at the fixed program paths.
func startSSHGitServer(t *testing.T, dir string) *sshGitServer {
	t.Helper()
	hostKey := filepath.Join(dir, "host_ed25519")
	clientKey := filepath.Join(dir, "client_ed25519")
	if _, err := os.Stat(hostKey); err != nil {
		runCmd(t, dir, nil, "/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	}
	if _, err := os.Stat(clientKey); err != nil {
		runCmd(t, dir, nil, "/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "",
			"-C", secretMarker, "-f", clientKey)
	}
	clientPub, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	hostPub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(dir, "authorized_keys")
	authorizedLine := fmt.Sprintf(
		`command="/usr/bin/git-shell -c \"$SSH_ORIGINAL_COMMAND\"",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty %s`,
		strings.TrimSpace(string(clientPub)))
	if err := os.WriteFile(authorized, []byte(authorizedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	config := filepath.Join(dir, "sshd_config")
	cfg := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PermitRootLogin prohibit-password
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
LogLevel VERBOSE
`, port, hostKey, filepath.Join(dir, "sshd.pid"), authorized)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "sshd.log")
	process := exec.Command(sshdBinary, "-D", "-e", "-f", config)
	process.Stderr = func() *os.File {
		f, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		return f
	}()
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Signal(syscall.SIGTERM)
		_ = process.Wait()
	})
	waitPort(t, port)

	// Install the pinned SSH material at the fixed program paths.
	sshDir := "/etc/git-repo-backup/ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSecret(filepath.Join(sshDir, "id"), decodeKey(t, clientKey)); err != nil {
		t.Fatal(err)
	}
	known := fmt.Sprintf("[127.0.0.1]:%d %s\n", port, strings.TrimSpace(string(hostPub)))
	if err := writeSecret(filepath.Join(sshDir, "known_hosts"), []byte(known)); err != nil {
		t.Fatal(err)
	}
	return &sshGitServer{port: port, rootDir: dir, process: process}
}

// decodeKey reads a generated private key file.
func decodeKey(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeSecret(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o400)
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sshd did not start listening")
}

// buildBinary compiles the backup binary.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git-repo-backup")
	srcRoot, err := filepath.Abs("../")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(t, srcRoot, nil, "go", "build", "-o", bin, "./cmd/git-repo-backup")
	return bin
}
