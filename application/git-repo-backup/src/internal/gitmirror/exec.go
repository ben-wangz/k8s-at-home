// Package gitmirror drives the system Git and OpenSSH binaries with a fully
// controlled environment, implements mirror clone/update, HEAD
// synchronization, fsck, and the persistent mirror cache.
package gitmirror

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
)

// SSHOptions controls the non-interactive OpenSSH policy. Paths are quoted
// before being placed in GIT_SSH_COMMAND, which Git executes through a shell.
type SSHOptions struct {
	PrivateKeyFile string
	KnownHostsFile string
	HostKeyPolicy  string
}

// BuildSSHCommand returns the hermetic OpenSSH command used by Git. The
// accept-new default implements TOFU: new keys are persisted, changed keys
// are rejected on later connections.
func BuildSSHCommand(options SSHOptions) (string, error) {
	policy := options.HostKeyPolicy
	if policy == "" {
		policy = "accept-new"
	}
	privateKey := options.PrivateKeyFile
	if privateKey == "" {
		privateKey = "/etc/git-repo-backup/ssh/id"
	}
	knownHosts := options.KnownHostsFile
	if knownHosts == "" {
		knownHosts = "/etc/git-repo-backup/ssh-state/state/known_hosts"
	}
	strict := policy
	switch policy {
	case "pinned":
		strict = "yes"
	case "accept-new":
		strict = "accept-new"
	case "none":
		strict = "no"
		knownHosts = "/dev/null"
	default:
		return "", fmt.Errorf("unsupported SSH host key policy %q", policy)
	}
	return "/usr/bin/ssh -F /dev/null -i " + shellQuote(privateKey) +
		" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=" + strict +
		" -o UserKnownHostsFile=" + shellQuote(knownHosts) +
		" -o GlobalKnownHostsFile=/dev/null -o UpdateHostKeys=no" +
		" -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no" +
		" -o ForwardAgent=no -o ClearAllForwardings=yes -o PermitLocalCommand=no" +
		" -o ConnectTimeout=30 -o ServerAliveInterval=30 -o ServerAliveCountMax=3", nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// Output bounds keep remote-controlled output out of memory and logs.
const (
	maxStdout = 1 << 20 // 1 MiB for refs listings
	maxStderr = 8 << 10 // 8 KiB diagnostics
)

// Runner executes Git commands with a hermetic environment. It never
// inherits the caller environment.
type Runner struct {
	gitBinary string
	env       []string
	Timeout   time.Duration
	// hardening disables transports and hooks. Production runs keep the
	// default; in-package tests exercising local-path clones drop the
	// protocol part (production only ever talks SSH or HTTPS).
	hardening []string
}

// protocolFlags reduce the allowed transports to SSH and HTTPS.
var protocolFlags = []string{
	"-c", "protocol.allow=never",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.https.allow=always",
}

// hooksFlags disable repository hooks.
var hooksFlags = []string{"-c", "core.hooksPath=/dev/null"}

// NewRunner prepares the Git environment: pinned binary paths, empty HOME
// with an empty global config, terminal prompts disabled, protocol allowlist,
// and the selected SSH command. The process umask must already be restrictive.
func NewRunner(gitBinary string, homeDir string, timeout time.Duration, sshOptions ...SSHOptions) (*Runner, error) {
	if err := safefs.EnsureDir(homeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create git home: %w", err)
	}
	globalConfig := homeDir + "/.gitconfig"
	if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
		return nil, fmt.Errorf("create empty global git config: %w", err)
	}
	options := SSHOptions{}
	if len(sshOptions) > 0 {
		options = sshOptions[0]
	}
	sshCommand, err := BuildSSHCommand(options)
	if err != nil {
		return nil, err
	}
	env := []string{
		"HOME=" + homeDir,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + globalConfig,
		"GIT_SSH_COMMAND=" + sshCommand,
	}
	hardening := append(append([]string{}, protocolFlags...), hooksFlags...)
	return &Runner{gitBinary: gitBinary, env: env, Timeout: timeout, hardening: hardening}, nil
}

// Run executes one Git command in its own process group. On timeout the whole
// group is terminated (TERM, short wait, KILL). Errors are classified safe
// codes; raw stderr is kept only in the wrapped error for internal use.
func (r *Runner) Run(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, r.hardening...), args...)
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.gitBinary, full...)
	cmd.Env = r.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &boundedWriter{b: &stdout, max: maxStdout}
	cmd.Stderr = &boundedWriter{b: &stderr, max: maxStderr}
	cmd.Cancel = func() error { return signalGroup(cmd) }
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	timedOut := ctx.Err() != nil
	code := observability.ClassifyGitStderr(stderr.String(), timedOut)
	detail := fmt.Sprintf("git %s failed (exit=%v)", args[0], exitCodeOf(err))
	return stdout.String(), observability.WrapSafe(code, detail, fmt.Errorf("%s", stderr.String()))
}

// signalGroup sends TERM to the process group, waits briefly, then KILLs.
func signalGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) == syscall.ESRCH {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return nil
}

func exitCodeOf(err error) any {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return "?"
}

// boundedWriter caps a buffer, discarding bytes beyond the limit.
type boundedWriter struct {
	b   *bytes.Buffer
	max int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	room := w.max - w.b.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		w.b.Write(p[:room])
		return len(p), nil
	}
	return w.b.Write(p)
}
