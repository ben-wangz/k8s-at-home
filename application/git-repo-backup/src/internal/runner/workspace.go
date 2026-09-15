package runner

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
	"git-repo-backup/internal/storage"
)

// workspace manages the scratch volume: a non-blocking root lock (so runs
// and cleanup never interleave when the volume is a shared PVC), the
// per-run directory <root>/.runs/<backup-id>-<pod-uid>, and age-based
// cleanup of leftovers from lost runs.
type workspace struct {
	root   string
	lock   *safefs.Lock
	runDir string
}

const runsDir = ".runs"

func openWorkspace(root string, minFreeBytes uint64) (*workspace, error) {
	if err := safefs.EnsureDir(root, 0o700); err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed, "prepare workspace root", err)
	}
	if err := safefs.CheckCapacity(root, minFreeBytes); err != nil {
		return nil, observability.WrapSafe(observability.CodeCapacity, "workspace capacity preflight", err)
	}
	lock, err := safefs.TryLock(filepath.Join(root, ".workspace.lock"))
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed,
			"workspace lock unavailable (another run active on this volume)", err)
	}
	return &workspace{root: root, lock: lock}, nil
}

// beginRun creates the exclusive run directory.
func (w *workspace) beginRun(backupID, podUID string) error {
	runs := filepath.Join(w.root, runsDir)
	if err := safefs.EnsureDir(runs, 0o700); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "prepare runs directory", err)
	}
	runDir := filepath.Join(runs, backupID+"-"+podUID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "create run directory", err)
	}
	w.runDir = runDir
	return nil
}

// mirrorsDir returns the ephemeral mirror directory inside the run dir.
func (w *workspace) mirrorsDir() string {
	return filepath.Join(w.runDir, "mirrors")
}

// cleanupStale removes run directories whose backup IDs are older than the
// incomplete cutoff; unparseable names are ignored, never deleted.
func (w *workspace) cleanupStale(now time.Time, incompleteMaxAge, maxRunGrace time.Duration) {
	entries, err := os.ReadDir(filepath.Join(w.root, runsDir))
	if err != nil {
		return
	}
	minAge := incompleteMaxAge
	if maxRunGrace > minAge {
		minAge = maxRunGrace
	}
	for _, e := range entries {
		id := stagingRunID(e.Name())
		if id == "" {
			continue
		}
		startedAt, err := storage.ParseBackupID(id)
		if err != nil || now.Sub(startedAt) <= minAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(w.root, runsDir, e.Name()))
	}
}

func stagingRunID(name string) string {
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		return name[:idx]
	}
	return ""
}

// close removes this run's directory and releases the lock.
func (w *workspace) close() {
	if w.runDir != "" {
		_ = os.RemoveAll(w.runDir)
	}
	// Best-effort sync; capability gaps and I/O errors are already
	// surfaced wherever durability of published data is at stake.
	_ = safefs.SyncDir(filepath.Join(w.root, runsDir))
	_ = w.lock.Close()
}
