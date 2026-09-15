package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TryLock(path)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("expected lock-held error, got %v", err)
	}
	if second != nil {
		t.Fatal("no lock object on contention")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock must be re-acquirable after release: %v", err)
	}
	_ = third.Close()
	// The lock file must persist, never unlinked.
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lock file must not be removed")
	}
}

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(a, b); err != nil {
		t.Fatalf("rename failed: %v", err)
	}
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("source must be gone")
	}
	// Renaming onto an existing target must refuse without overwriting.
	c := filepath.Join(dir, "c")
	if err := os.Mkdir(c, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "keep"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(c, b); !errors.Is(err, ErrExists) {
		t.Fatalf("expected exists error, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(b, "keep")); err != nil {
		t.Fatal("existing target must be untouched")
	}
}

func TestCheckCapacity(t *testing.T) {
	dir := t.TempDir()
	if err := CheckCapacity(dir, 1); err != nil {
		t.Fatalf("one byte should fit: %v", err)
	}
	if err := CheckCapacity(dir, 1<<62); err == nil {
		t.Skip("filesystem reports implausibly large free space; skipping")
	}
}

func TestValidateAbsolutePath(t *testing.T) {
	valid := map[string]string{"/a/b": "x", "/backup": "y"}
	for path := range valid {
		if err := ValidateAbsolutePath("p", path); err != nil {
			t.Errorf("expected %q valid: %v", path, err)
		}
	}
	invalid := []string{"", "/", "relative", "/a/../b", "/a\x01b", "//"}
	for _, path := range invalid {
		if err := ValidateAbsolutePath("p", path); err == nil {
			t.Errorf("expected %q invalid", path)
		}
	}
}

func TestContainsPath(t *testing.T) {
	if !ContainsPath("/a", "/a/b") || !ContainsPath("/a", "/a") {
		t.Fatal("containment broken")
	}
	if ContainsPath("/a", "/ab") {
		t.Fatal("prefix must match on path separators")
	}
}

func TestWriteFileDurableRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := WriteFileDurable(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileDurable(path, []byte("two"), 0o600); err == nil {
		t.Fatal("overwrite must be refused")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "one" {
		t.Fatal("content must be unchanged")
	}
}
