package gitmirror

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"git-repo-backup/internal/observability"
)

// RemoteHead describes the HEAD the remote advertises right now.
type RemoteHead struct {
	HasHead  bool
	Unborn   bool   // HEAD points at a branch with no commits yet
	Symbolic bool   // HEAD is refs/heads/<branch>
	Ref      string // symbolic target, e.g. refs/heads/main
	OID      string // advertised OID (set for detached and unborn targets)
}

// LsRemoteHead fetches the remote symbolic HEAD via ls-remote --symref.
func (r *Runner) LsRemoteHead(ctx context.Context, url string) (RemoteHead, error) {
	out, err := r.Run(ctx, "ls-remote", "--symref", "--", url, "HEAD")
	if err != nil {
		return RemoteHead{}, err
	}
	return ParseLsRemoteHead(out)
}

// ParseLsRemoteHead parses ls-remote --symref output for the HEAD query.
// Typical shapes per line, tab-separated:
//
//	ref: refs/heads/main\tHEAD
//	<oid>\tHEAD
func ParseLsRemoteHead(out string) (RemoteHead, error) {
	head := RemoteHead{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ref: ") {
			parts := strings.SplitN(line[5:], "\t", 2)
			if len(parts) != 2 || parts[1] != "HEAD" {
				return RemoteHead{}, fmt.Errorf("unexpected symref line")
			}
			head.HasHead = true
			head.Symbolic = true
			head.Ref = parts[0]
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || parts[1] != "HEAD" {
			continue
		}
		head.HasHead = true
		if head.OID != "" && head.OID != parts[0] {
			return RemoteHead{}, fmt.Errorf("conflicting HEAD oids")
		}
		head.OID = parts[0]
	}
	if head.HasHead && head.Symbolic && isNullOID(head.OID) {
		head.Unborn = true
	}
	return head, nil
}

func isNullOID(oid string) bool {
	for _, c := range oid {
		if c != '0' {
			return false
		}
	}
	return len(oid) > 0
}

// SyncHead aligns the local bare mirror HEAD with the remote HEAD. It is run
// after every clone or fetch because fetch does not update symbolic HEAD.
// For an unborn (empty-repository) HEAD the advertised symbolic target is
// applied when the server reports it — still unborn, never fabricated —
// and otherwise the HEAD git clone produced is kept.
func (r *Runner) SyncHead(ctx context.Context, dir, url string, allowRefetch bool) error {
	remote, err := r.LsRemoteHead(ctx, url)
	if err != nil {
		return err
	}
	if !remote.HasHead {
		return nil
	}
	if remote.Unborn {
		if remote.Ref == "" {
			return nil
		}
		if _, err := r.Run(ctx, "check-ref-format", remote.Ref); err != nil {
			return observability.WrapSafe(observability.CodeGitFailed,
				"remote unborn HEAD target failed ref-format validation", nil)
		}
		return r.setSymbolicHead(ctx, dir, remote.Ref)
	}
	if remote.Symbolic {
		return r.syncSymbolicHead(ctx, dir, url, remote.Ref, allowRefetch)
	}
	return r.syncDetachedHead(ctx, dir, remote.OID)
}

func (r *Runner) syncSymbolicHead(ctx context.Context, dir, url, ref string, allowRefetch bool) error {
	if strings.HasPrefix(ref, "-") {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD target failed ref-format validation", nil)
	}
	if _, err := r.Run(ctx, "check-ref-format", ref); err != nil {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD target failed ref-format validation", nil)
	}
	if err := r.setSymbolicHead(ctx, dir, ref); err != nil {
		return err
	}
	exists, err := r.refExists(ctx, dir, ref)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if !allowRefetch {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD branch missing from mirror", nil)
	}
	// The default branch moved after our fetch; fetch once more, then fail
	// if it is still absent rather than shipping a known-incomplete HEAD.
	if err := r.RemoteUpdate(ctx, dir, url); err != nil {
		return err
	}
	exists, err = r.refExists(ctx, dir, ref)
	if err != nil {
		return err
	}
	if !exists {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD branch missing from mirror after refetch", nil)
	}
	return nil
}

func (r *Runner) setSymbolicHead(ctx context.Context, dir, ref string) error {
	_, err := r.Run(ctx, "-C", dir, "symbolic-ref", "HEAD", ref)
	return err
}

func (r *Runner) syncDetachedHead(ctx context.Context, dir, oid string) error {
	if !isHexOID(oid) {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD oid has invalid format", nil)
	}
	if err := r.oidExists(ctx, dir, oid); err != nil {
		return err
	}
	_, err := r.Run(ctx, "-C", dir, "update-ref", "--no-deref", "HEAD", oid)
	return err
}

func (r *Runner) refExists(ctx context.Context, dir, ref string) (bool, error) {
	out, err := r.Run(ctx, "-C", dir, "show-ref", "--verify", ref)
	if err != nil {
		return false, nil // show-ref exits non-zero when the ref is absent
	}
	return strings.TrimSpace(out) != "", nil
}

func (r *Runner) oidExists(ctx context.Context, dir, oid string) error {
	if _, err := r.Run(ctx, "-C", dir, "cat-file", "-e", oid+"^{object}"); err != nil {
		return observability.WrapSafe(observability.CodeGitFailed,
			"remote HEAD object missing from mirror", nil)
	}
	return nil
}

// isHexOID accepts 40 (SHA-1) or 64 (SHA-256) lowercase or uppercase hex.
func isHexOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// HeadState reports the local mirror HEAD for manifest recording.
type HeadState struct {
	Symbolic bool
	Ref      string // empty for detached
	OID      string // empty for unborn symbolic HEAD
}

// LocalHead reads the mirror HEAD without following symbolic refs twice.
func (r *Runner) LocalHead(ctx context.Context, dir string) (HeadState, error) {
	out, err := r.Run(ctx, "-C", dir, "symbolic-ref", "-q", "HEAD")
	if err == nil {
		ref := strings.TrimSpace(out)
		oid := ""
		if oidOut, err := r.Run(ctx, "-C", dir, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
			oid = strings.TrimSpace(oidOut)
		}
		return HeadState{Symbolic: true, Ref: ref, OID: oid}, nil
	}
	oidOut, err := r.Run(ctx, "-C", dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return HeadState{}, nil // unborn or missing; nothing to record
	}
	return HeadState{Symbolic: false, OID: strings.TrimSpace(oidOut)}, nil
}

// Versions returns the git version string for startup logging.
func (r *Runner) Versions(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "--version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) >= 3 {
		return fields[2], nil
	}
	return strconv.Quote(strings.TrimSpace(out)), nil
}
