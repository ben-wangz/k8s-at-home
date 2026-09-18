package prepare

import (
	"os"
	"path/filepath"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
)

// prepareKnownHostsState creates or validates the writable known_hosts file
// used by accept-new. The state directory is a child of the mounted claim so
// filesystem-created entries such as lost+found do not become application
// data. Existing non-empty state is never replaced or reseeded.
func (p *preparer) prepareKnownHostsState() error {
	path := p.cfg.Prepare.KnownHostsStateFile
	dir := filepath.Dir(path)
	parent := filepath.Dir(dir)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "known_hosts state volume missing", err)
	}
	if !parentInfo.IsDir() {
		return observability.WrapSafe(observability.CodePrepareFailed, "known_hosts state volume is not a directory", nil)
	}
	info, err := os.Lstat(dir)
	created := false
	if os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return observability.WrapSafe(observability.CodePrepareFailed, "create known_hosts state directory", err)
		}
		created = true
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "inspect known_hosts state directory", err)
	}
	if !info.IsDir() {
		return observability.WrapSafe(observability.CodePrepareFailed, "known_hosts state path is not a directory", nil)
	}
	if !created && ownedByTarget(info, p.cfg) && info.Mode().Perm() == 0o700 {
		// The target-owned directory is intentionally not traversed here. The
		// init container has only CHOWN and cannot read a 0700 directory owned
		// by the workload UID; the run container validates the state file.
		return nil
	}
	empty, err := safefs.IsEmptyDir(dir)
	if err != nil {
		return observability.WrapSafe(observability.CodePrepareFailed, "inspect known_hosts state directory", err)
	}
	if empty {
		var data []byte
		if p.cfg.Prepare.InputKnownHostsFile != "" {
			data, err = readInputFile(p.cfg.Prepare.InputKnownHostsFile)
			if err != nil {
				return err
			}
		}
		if err := writeOwnedFile(path, data, 0o600, p.cfg); err != nil {
			return err
		}
		return handOverDir(dir, p.cfg)
	}
	if ownedByTarget(info, p.cfg) {
		return observability.WrapSafe(observability.CodePrepareFailed,
			"known_hosts state directory has unexpected mode", nil)
	}
	return observability.WrapSafe(observability.CodePrepareFailed,
		"known_hosts state directory is not owned by the target uid; provision manually", nil)
}
