package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"git-repo-backup/internal/gitmirror"
)

// buildMirror creates a fake bare mirror tree with typical Git files.
func buildMirror(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"HEAD":                     "ref: refs/heads/main\n",
		"config":                   "[core]\n\tbare = true\n",
		"packed-refs":              "# pack-refs\n",
		"refs/heads/main":          "0123456789012345678901234567890123456789\n",
		"hooks/pre-receive.sample": "#!/bin/sh\n",
		"objects/info/packs":       "",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readEntries(t *testing.T, path string) []tar.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var out []tar.Header
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *hdr)
	}
}

func TestCreateArchiveLayout(t *testing.T) {
	mirror := buildMirror(t)
	dest := filepath.Join(t.TempDir(), "repo.tar.gz")
	cfgBytes := gitmirror.RenderBareConfig(gitmirror.ConfigInfo{
		RepositoryFormatVersion: "0", RemoteURL: gitmirror.PlaceholderRemoteURL,
	})
	res, err := Create(dest, mirror, "repo", 6, cfgBytes)
	if err != nil {
		t.Fatal(err)
	}
	// Re-read and compare with the recorded digest.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("recorded digest does not match file contents")
	}
	if res.Size != int64(len(data)) {
		t.Fatal("recorded size does not match file contents")
	}
	headers := readEntries(t, dest)
	var names []string
	configSeen := false
	for _, h := range headers {
		names = append(names, h.Name)
		if !strings.HasPrefix(h.Name, "repo.git/") {
			t.Fatalf("entry outside top-level dir: %s", h.Name)
		}
		if strings.Contains(h.Name, "/../") || strings.HasPrefix(h.Name, "/") {
			t.Fatalf("unsafe entry name: %s", h.Name)
		}
		if h.Name == "repo.git/config" {
			configSeen = true
		}
		if strings.Contains(h.Name, "hooks/") {
			t.Fatalf("hooks must not be archived: %s", h.Name)
		}
		if h.Uname != "" || h.Gname != "" || h.Uid != 0 || h.Gid != 0 {
			t.Fatalf("numeric ownership must be normalized: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Mode != 0o700 {
				t.Fatalf("dir mode must be 0700: %s", h.Name)
			}
		case tar.TypeReg:
			if h.Mode != 0o600 {
				t.Fatalf("file mode must be 0600: %s", h.Name)
			}
		default:
			t.Fatalf("unexpected entry type: %s %v", h.Name, h.Typeflag)
		}
	}
	if !configSeen {
		t.Fatal("normalized config missing from archive")
	}
	// Deterministic ordering: lexicographic within each directory.
	sorted := append([]string(nil), names...)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] > sorted[i] && filepath.Dir(sorted[i-1]) == filepath.Dir(sorted[i]) {
			t.Fatalf("entries not lexicographically ordered: %v", names)
		}
	}
	// The archive config must contain the placeholder URL, not a real one.
	f, _ := os.Open(dest)
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "repo.git/config" {
			body, _ := io.ReadAll(tr)
			if !strings.Contains(string(body), "restore.invalid") {
				t.Fatalf("archive config lacks placeholder URL: %s", body)
			}
		}
	}
}

func TestCreateArchiveRejectsUnsafe(t *testing.T) {
	cfgBytes := gitmirror.RenderBareConfig(gitmirror.ConfigInfo{RepositoryFormatVersion: "0"})
	cases := []struct {
		name  string
		setup func(root string)
	}{
		{"symlink", func(root string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(root, "evil"))
		}},
		{"lock file", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "config.lock"), []byte("x"), 0o600)
		}},
		{"alternates", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "objects", "info", "alternates"), []byte("/other"), 0o600)
		}},
		{"fifo", func(root string) {
			_ = syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mirror := buildMirror(t)
			tc.setup(mirror)
			dest := filepath.Join(t.TempDir(), "repo.tar.gz")
			if _, err := Create(dest, mirror, "repo", 6, cfgBytes); err == nil {
				t.Fatal("expected rejection")
			}
			if _, err := os.Stat(dest + ".tmp"); err == nil {
				t.Fatal("temp file must not survive a failed archive")
			}
		})
	}
}
