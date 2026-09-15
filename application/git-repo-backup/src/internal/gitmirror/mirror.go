package gitmirror

import (
	"context"
	"strings"
)

// Clone creates a fresh bare mirror of url at dir.
func (r *Runner) Clone(ctx context.Context, url, dir string) error {
	if _, err := r.Run(ctx, "clone", "--mirror", "--", url, dir); err != nil {
		return err
	}
	return nil
}

// RemoteUpdate points origin at url and performs a pruning mirror fetch.
// The URL grammar is validated at configuration load and cannot start with
// '-', so it is safe as a plain argument.
func (r *Runner) RemoteUpdate(ctx context.Context, dir, url string) error {
	if _, err := r.Run(ctx, "-C", dir, "remote", "set-url", "origin", url); err != nil {
		return err
	}
	_, err := r.Run(ctx, "-C", dir, "remote", "update", "--prune")
	return err
}

// Fsck runs a full object connectivity and validity check on the mirror.
func (r *Runner) Fsck(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, "-C", dir, "fsck", "--full")
	return err
}

// CountRefs counts real refs, excluding HEAD itself and peeled tag entries.
func (r *Runner) CountRefs(ctx context.Context, dir string) (int, error) {
	out, err := r.Run(ctx, "-C", dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}
