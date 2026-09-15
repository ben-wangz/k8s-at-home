package local

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir(), 1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func identity(id string) storage.RunIdentity {
	return storage.RunIdentity{BackupID: id, OwnerUUID: "owner", StartedAt: time.Now().UTC()}
}

// publish performs a full run through the store; Begin conflicts are
// returned to the caller like any other phase error.
func publish(t *testing.T, st *Store, id string, marker manifest.SuccessMarker) error {
	t.Helper()
	if err := st.Begin(context.Background(), identity(id)); err != nil {
		return err
	}
	archivePath := st.ArchivePath("repo")
	if err := os.WriteFile(archivePath, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutArchive(context.Background(), storage.Artifact{
		Name: "repo", Path: archivePath, SHA256: "digest", Size: int64(len("archive")), RefCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMetadata(context.Background(), []byte("manifest"), []byte("checksums")); err != nil {
		t.Fatal(err)
	}
	return st.Commit(context.Background(), marker)
}

func TestPublishLifecycle(t *testing.T) {
	st := newTestStore(t)
	marker := manifest.SuccessMarker{SchemaVersion: 1, BackupID: "20260914T020000Z",
		ManifestSHA256: digestOf("manifest"), ChecksumsSHA256: digestOf("checksums")}
	if err := publish(t, st, "20260914T020000Z", marker); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(st.root, dirBackups, "20260914T020000Z")
	for _, name := range []string{manifest.ManifestName, manifest.ChecksumsName, manifest.MarkerName, "repositories/repo.tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// Staging must be empty after publication.
	entries, _ := os.ReadDir(filepath.Join(st.root, dirStaging))
	if len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v", entries)
	}
	// A second run with the same ID must refuse without overwriting.
	if err := publish(t, st, "20260914T020000Z", marker); err == nil {
		t.Fatal("same-second collision must fail")
	}
}

func TestAbortRemovesStaging(t *testing.T) {
	st := newTestStore(t)
	if err := st.Begin(context.Background(), identity("20260914T030000Z")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.ArchivePath("repo"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(st.root, dirStaging))
	if len(entries) != 0 {
		t.Fatalf("abort must remove staging: %v", entries)
	}
	entries, _ = os.ReadDir(filepath.Join(st.root, dirBackups))
	if len(entries) != 0 {
		t.Fatalf("abort must not publish: %v", entries)
	}
}

func TestCatalogLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"20260914T010000Z", "20260914T020000Z", "20260914T030000Z"} {
		marker := manifest.SuccessMarker{SchemaVersion: 1, BackupID: id,
			ManifestSHA256: digestOf("m"), ChecksumsSHA256: digestOf("c")}
		if err := publish(t, st, id, marker); err != nil {
			t.Fatal(err)
		}
		// A real manifest is required for completeness classification.
		mpath := filepath.Join(st.root, dirBackups, id, manifest.ManifestName)
		body := []byte(`{"schemaVersion":1,"backupId":"` + id + `","repositories":[]}`)
		if err := os.WriteFile(mpath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An incomplete run: staging leftover with a valid ID prefix.
	stagingName := "20260913T000000Z-stale"
	if err := os.Mkdir(filepath.Join(st.root, dirStaging, stagingName), 0o700); err != nil {
		t.Fatal(err)
	}
	// Unrelated names are ignored forever.
	if err := os.Mkdir(filepath.Join(st.root, dirBackups, "random-junk"), 0o700); err != nil {
		t.Fatal(err)
	}

	backups, err := st.ListBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 3 {
		t.Fatalf("expected 3 backups, got %+v", backups)
	}
	for _, b := range backups {
		if !b.Complete {
			t.Fatalf("expected complete: %+v", b)
		}
	}

	// Delete the oldest; trash path must clean it fully.
	if err := st.DeleteBackup(ctx, backups[len(backups)-1]); err != nil {
		t.Fatal(err)
	}
	backups, _ = st.ListBackups(ctx)
	if len(backups) != 2 {
		t.Fatalf("expected 2 after deletion, got %+v", backups)
	}

	incomplete, err := st.ListIncomplete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(incomplete) != 1 || incomplete[0].ID != "20260913T000000Z" {
		t.Fatalf("unexpected incomplete list: %+v", incomplete)
	}
	if err := st.DeleteIncomplete(ctx, incomplete[0]); err != nil {
		t.Fatal(err)
	}
	if incomplete, _ = st.ListIncomplete(ctx); len(incomplete) != 0 {
		t.Fatalf("incomplete not cleaned: %+v", incomplete)
	}
	// The junk dir must never be touched.
	if _, err := os.Stat(filepath.Join(st.root, dirBackups, "random-junk")); err != nil {
		t.Fatal("unrelated directories must not be deleted")
	}
}

func TestLockExcludesSecondStore(t *testing.T) {
	root := t.TempDir()
	first, err := New(root, 1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	if _, err := New(root, 1, slog.Default()); err == nil {
		t.Fatal("second store on the same root must fail on the lock")
	}
}

func digestOf(s string) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hexDigits[(s[0]+byte(i))%16]
	}
	return string(out)
}
