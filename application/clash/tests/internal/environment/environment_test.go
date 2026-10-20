package environment

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestLocalLockAndInvalidIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := LocalLock("lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if second, err := LocalLock("lock-test"); err == nil {
		second()
		t.Fatal("concurrent lock acquired")
	}
	for _, id := range []string{"../outside", "", "UPPER", "a/b", "a;delete"} {
		if _, err := LocalLock(id); err == nil {
			t.Fatalf("invalid ID accepted: %q", id)
		}
	}
}

func TestLeaseTimeUsesKubernetesMicrosecondPrecision(t *testing.T) {
	value := leaseTime()
	if !regexp.MustCompile(`\.\d{6}Z$`).MatchString(value) {
		t.Fatalf("lease time must use six fractional digits: %s", value)
	}
	if _, err := time.Parse(leaseTimeLayout, value); err != nil {
		t.Fatalf("lease time is invalid: %v", err)
	}
}

func TestCleanupRejectsChangedUID(t *testing.T) {
	directory := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in\n  *' get '*) printf '%s' '{\"kind\":\"ConfigMap\",\"metadata\":{\"uid\":\"new-owner\"}}' ;;\n  *) touch \"" + filepath.Join(directory, "deleted") + "\" ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(directory, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := Environment{App: directory, Context: "test", Namespace: "develop"}
	if err := env.deleteOwned(context.Background(), Resource{Kind: "ConfigMap", Name: "owned", UID: "old-owner"}, "lock-test"); err == nil {
		t.Fatal("changed UID accepted")
	}
	if _, err := os.Stat(filepath.Join(directory, "deleted")); !os.IsNotExist(err) {
		t.Fatal("cleanup invoked delete on a changed resource")
	}
}

func TestCleanupRefusesClusterScopedResources(t *testing.T) {
	for _, kind := range []string{"Namespace", "PersistentVolume", "StorageClass", "ClusterRole"} {
		if _, err := ResourcePath("develop", Resource{Kind: kind, Name: "test"}); err == nil {
			t.Fatalf("cluster-scoped resource accepted: %s", kind)
		}
	}
}

func TestSourceIdentityIgnoresBuildCachesAndRejectsSymlinks(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "source.ts"), []byte("export const ready = true;"), 0600); err != nil {
		t.Fatal(err)
	}
	initial, err := SourceIdentity(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "node_modules/cache"), []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := SourceIdentity(directory)
	if err != nil || current != initial {
		t.Fatal("build cache changed source identity")
	}
	if err := os.Symlink("source.ts", filepath.Join(directory, "linked.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := SourceIdentity(directory); err == nil {
		t.Fatal("source symlink accepted")
	}
}
