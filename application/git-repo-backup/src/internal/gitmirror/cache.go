package gitmirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
)

// Cache is the persistent mirror cache working state. It is guarded by a
// non-blocking flock on <root>/.lock held for the whole run; a competing
// pod or manual job fails fast instead of mutating concurrently.
type Cache struct {
	root string
	lock *safefs.Lock
}

// OpenCache prepares the cache layout and acquires the exclusive lock.
func OpenCache(root string) (*Cache, error) {
	if err := safefs.EnsureDir(filepath.Join(root, "mirrors"), 0o700); err != nil {
		return nil, err
	}
	if err := safefs.EnsureDir(filepath.Join(root, "state"), 0o700); err != nil {
		return nil, err
	}
	lock, err := safefs.TryLock(filepath.Join(root, ".lock"))
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed,
			"cache lock unavailable", err)
	}
	return &Cache{root: root, lock: lock}, nil
}

// Close releases the cache lock. The lock file itself is kept.
func (c *Cache) Close() error { return c.lock.Close() }

// CleanupTemp removes cache clone temporary directories older than maxAge,
// e.g. left behind by a killed rebuild. Must be called while holding the
// cache lock.
func (c *Cache) CleanupTemp(now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(filepath.Join(c.root, "mirrors"))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(c.root, "mirrors", name))
	}
}

func (c *Cache) mirrorPath(name string) string {
	return filepath.Join(c.root, "mirrors", name+".git")
}

func (c *Cache) statePath(name string) string {
	return filepath.Join(c.root, "state", name+".json")
}

// PrepareMirror produces a verified mirror for name/url: clone or fetch,
// HEAD synchronization, then fsck. With no cache it clones into ephemeralDir
// and reports ephemeral=true so the caller removes it after archiving.
func (c *Cache) PrepareMirror(ctx context.Context, r *Runner, name, url, ephemeralDir string) (string, bool, error) {
	if c == nil {
		dir := filepath.Join(ephemeralDir, name+".git")
		if err := buildMirror(ctx, r, url, dir, false); err != nil {
			return "", false, err
		}
		return dir, true, nil
	}
	if err := c.updateEntry(ctx, r, name, url); err == nil {
		return c.mirrorPath(name), false, nil
	} else if isFatalImmediately(err) {
		return "", false, err
	}
	// One rebuild covers structural damage, update failures, fsck failures,
	// and credential or URL changes; a second failure aborts the run.
	if err := c.rebuildEntry(ctx, r, name, url); err != nil {
		return "", false, err
	}
	return c.mirrorPath(name), false, nil
}

// isFatalImmediately skips the rebuild attempt for timeouts and
// cancellation, which a re-clone would not rescue.
func isFatalImmediately(err error) bool {
	switch observability.CodeOf(err) {
	case observability.CodeGitTimeout, observability.CodeCanceled, observability.CodeDeadlineExceeded:
		return true
	}
	return false
}

// updateEntry validates, refreshes, syncs HEAD, and fscks an existing cache
// entry in place.
func (c *Cache) updateEntry(ctx context.Context, r *Runner, name, url string) error {
	dir := c.mirrorPath(name)
	st, err := readCacheState(c.statePath(name))
	if err != nil {
		return err
	}
	if st.URLFingerprint != fingerprint(url) {
		return fmt.Errorf("cache fingerprint mismatch")
	}
	if err := r.ValidateStructure(ctx, dir); err != nil {
		return err
	}
	if err := r.NormalizeConfig(ctx, dir, url); err != nil {
		return err
	}
	if err := r.RemoteUpdate(ctx, dir, url); err != nil {
		return err
	}
	if err := r.SyncHead(ctx, dir, url, false); err != nil {
		return err
	}
	return r.Fsck(ctx, dir)
}

// rebuildEntry discards the cache entry and clones once into a temporary
// directory, promoting it only after the fresh clone verifies.
func (c *Cache) rebuildEntry(ctx context.Context, r *Runner, name, url string) error {
	tmp := filepath.Join(c.root, "mirrors", ".tmp-"+name+".git")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := buildMirror(ctx, r, url, tmp, true); err != nil {
		return err
	}
	final := c.mirrorPath(name)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return writeCacheState(c.statePath(name), fingerprint(url))
}

// buildMirror clones url into dir, syncs HEAD, fscks, and (for cache
// entries) normalizes the config so future updates keep working. Ephemeral
// mirrors keep the clone-default config; the archive step rewrites it.
func buildMirror(ctx context.Context, r *Runner, url, dir string, forCache bool) error {
	if err := r.Clone(ctx, url, dir); err != nil {
		return err
	}
	if err := r.SyncHead(ctx, dir, url, false); err != nil {
		return err
	}
	if err := r.Fsck(ctx, dir); err != nil {
		return err
	}
	if forCache {
		return r.NormalizeConfig(ctx, dir, url)
	}
	return nil
}
