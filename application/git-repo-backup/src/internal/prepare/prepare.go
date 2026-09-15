// Package prepare implements the root-restricted initContainer setup: it
// copies the projected SSH secret and known_hosts into a memory emptyDir
// with correct ownership, and hands fresh empty volumes to the backup UID.
// It never runs Git, SSH, or any network call.
package prepare

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
)

// Run prepares the SSH volume and managed volume roots. It is idempotent:
// an already-prepared volume is verified at its root and skipped.
func Run(cfg *config.Config) error {
	p := preparer{cfg: cfg}
	if err := p.prepareSSH(); err != nil {
		return err
	}
	return p.prepareVolumes()
}

type preparer struct {
	cfg *config.Config
}

// prepareSSH copies the private key (0400) and known_hosts (0444) into a
// fresh empty volume, chowning files first and the directory last, so a
// CHOWN-only root process never loses access mid-setup. An already
// prepared volume (owned by the target uid, mode 0700) is verified at its
// root and skipped.
func (p *preparer) prepareSSH() error {
	dir := p.cfg.Prepare.SSHDir
	info, err := os.Lstat(dir)
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "prepared ssh volume missing", err)
	}
	if !info.IsDir() {
		return observability.WrapSafe(observability.CodePrepareFailed, "prepared ssh path is not a directory", nil)
	}
	empty, err := safefs.IsEmptyDir(dir)
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "inspect prepared ssh volume", err)
	}
	if !empty {
		if ownedByTarget(info, p.cfg) {
			return p.verifyPreparedRoot(dir, info)
		}
		return observability.WrapSafe(observability.CodePrepareFailed,
			"prepared ssh volume is not empty and not owned by the target uid; provision manually", nil)
	}
	keyData, err := readInputFile(p.cfg.Prepare.InputPrivateKeyFile)
	if err != nil {
		return err
	}
	knownData, err := readInputFile(p.cfg.Prepare.InputKnownHostsFile)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(knownData)) == 0 {
		return observability.WrapSafe(observability.CodePrepareFailed, "known_hosts input is empty", nil)
	}
	idPath := filepath.Join(dir, "id")
	if err := writeOwnedFile(idPath, keyData, 0o400, p.cfg); err != nil {
		return err
	}
	knownPath := filepath.Join(dir, "known_hosts")
	if err := writeOwnedFile(knownPath, knownData, 0o444, p.cfg); err != nil {
		return err
	}
	return handOverDir(dir, p.cfg)
}

// prepareVolumes gives fresh empty volume roots (workspace, backup, cache)
// and /tmp to the backup UID. Non-empty volumes must already belong to it;
// root-squash storage needs administrator-provided root directories.
func (p *preparer) prepareVolumes() error {
	roots := []string{p.cfg.Workspace.Root, p.cfg.Prepare.TempDir}
	if p.cfg.Storage.Type == "local" {
		roots = append(roots, p.cfg.Storage.Local.Root)
	}
	if p.cfg.Cache.Enabled {
		roots = append(roots, p.cfg.Cache.Root)
	}
	for _, root := range roots {
		if err := p.prepareVolumeRoot(root); err != nil {
			return err
		}
	}
	return nil
}

func (p *preparer) prepareVolumeRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		// A volume root that does not exist cannot be a mount point; do
		// not silently create arbitrary paths as root.
		return observability.WrapSafe(observability.CodePrepareFailed,
			"volume root missing (not mounted?)", err)
	}
	if !info.IsDir() {
		return observability.WrapSafe(observability.CodePrepareFailed, "volume root is not a directory", nil)
	}
	empty, err := safefs.IsEmptyDir(root)
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "inspect volume root", err)
	}
	if empty {
		// Brand-new volume: set target ownership and mode 0700.
		return handOverDir(root, p.cfg)
	}
	if ownedByTarget(info, p.cfg) {
		return nil
	}
	return observability.WrapSafe(observability.CodePrepareFailed,
		"volume root is not empty and not owned by the target uid; fix provisioning (root-squash storage needs an admin-created root)", nil)
}

// verifyPreparedRoot checks an already-handed-over volume without entering
// it: a CHOWN-only root process cannot read inside a 0700 directory owned
// by another user.
func (p *preparer) verifyPreparedRoot(dir string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o700 {
		return observability.WrapSafe(observability.CodePrepareFailed,
			"prepared ssh volume has unexpected mode", nil)
	}
	return nil
}

func ownedByTarget(info os.FileInfo, cfg *config.Config) bool {
	stat, ok := info.Sys().(*syscallStat)
	if !ok {
		return false
	}
	return int(stat.Uid) == cfg.Prepare.TargetUID && int(stat.Gid) == cfg.Prepare.TargetGID
}

func writeOwnedFile(path string, data []byte, mode os.FileMode, cfg *config.Config) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "write prepared file", err)
	}
	if err := os.Chown(path, cfg.Prepare.TargetUID, cfg.Prepare.TargetGID); err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed,
			"chown prepared file (missing CAP_CHOWN?)", err)
	}
	return nil
}

func handOverDir(dir string, cfg *config.Config) error {
	if err := os.Chown(dir, cfg.Prepare.TargetUID, cfg.Prepare.TargetGID); err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed,
			"chown volume root (root-squash storage needs an admin-created root)", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "chmod volume root", err)
	}
	return nil
}

// readInputFile reads a projected secret or configmap file. Projected
// volumes contain Kubernetes atomic-writer symlinks; these mount-time
// symlinks are the one trusted exception to the no-symlink rule.
func readInputFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, observability.WrapSafe(observability.CodePrepareFailed, "read projected input file", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, observability.WrapSafe(observability.CodePrepareFailed,
			fmt.Sprintf("projected input file %s is empty", filepath.Base(path)), nil)
	}
	return data, nil
}
