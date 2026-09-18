package gitmirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCachePrepareMirrorRebuildsOnCorruption(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	bare := seedBareRepo(t)

	cache, err := OpenCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	ephemeral := filepath.Join(t.TempDir(), "ephemeral")
	if _, _, err := cache.PrepareMirror(ctx, r, "repo", bare, ephemeral); err != nil {
		t.Fatalf("first cache population failed: %v", err)
	}
	// Corrupt the cache: wrong fingerprint triggers rebuild.
	mirror := cache.mirrorPath("repo")
	if err := os.WriteFile(filepath.Join(mirror, "packed-refs"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, isEphemeral, err := cache.PrepareMirror(ctx, r, "repo", bare, ephemeral)
	if err != nil {
		t.Fatalf("rebuild-once must recover: %v", err)
	}
	if isEphemeral || got != mirror {
		t.Fatalf("expected cache path after rebuild: %s", got)
	}
	if err := r.Fsck(ctx, mirror); err != nil {
		t.Fatalf("rebuilt cache must pass fsck: %v", err)
	}
}
