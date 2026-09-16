// Package runner drives the backup state machine: preflight, Begin,
// per-repository mirror+archive+store, metadata, commit, retention, and
// final summary.
package runner

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
)

// sshKeyGenBinary is the pinned ssh-keygen used for the non-interactive
// private-key parse check.
const sshKeyGenBinary = "/usr/bin/ssh-keygen"

// preflight validates the mounted SSH material without any network access.
// The private key must be a 0400 regular file owned by the current UID and
// parseable without a passphrase; encrypted keys are rejected because the
// job must never block on a prompt.
func preflight(cfg *config.Config) error {
	info, err := os.Lstat(cfg.SSH.PrivateKeyFile)
	if err != nil {
		return observability.WrapSafe(observability.CodeInputInvalid, "ssh private key file missing", err)
	}
	if !info.Mode().IsRegular() {
		return observability.WrapSafe(observability.CodeInputInvalid, "ssh private key is not a regular file", nil)
	}
	if info.Mode().Perm() != 0o400 {
		return observability.WrapSafe(observability.CodeInputInvalid,
			"ssh private key must have mode 0400", nil)
	}
	if !ownedByCurrentUser(info) {
		return observability.WrapSafe(observability.CodeInputInvalid,
			"ssh private key must be owned by the current uid", nil)
	}
	known, err := os.Lstat(cfg.SSH.KnownHostsFile)
	if err != nil {
		return observability.WrapSafe(observability.CodeInputInvalid, "known_hosts file missing", err)
	}
	if !known.Mode().IsRegular() || known.Size() == 0 {
		return observability.WrapSafe(observability.CodeInputInvalid,
			"known_hosts must be a non-empty regular file", nil)
	}
	if err := checkKeyParses(cfg.SSH.PrivateKeyFile); err != nil {
		return err
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscallStat)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}

// checkKeyParses derives the public key non-interactively; an encrypted key
// fails here instead of stalling the job.
func checkKeyParses(keyPath string) error {
	cmd := exec.Command(sshKeyGenBinary, "-y", "-P", "", "-f", keyPath)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return observability.WrapSafe(observability.CodeInputInvalid,
			"ssh private key is not usable (encrypted keys are not supported)", err)
	}
	return nil
}

// SSHVersion reports the OpenSSH client version for startup logging.
func SSHVersion() string {
	cmd := exec.Command("/usr/bin/ssh", "-V")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && stderr.Len() == 0 {
		return fmt.Sprintf("unknown")
	}
	out := stderr.String()
	for _, line := range bytes.Split([]byte(out), []byte("\n")) {
		if bytes.Contains(line, []byte("OpenSSH")) {
			return string(line)
		}
	}
	return "unknown"
}
