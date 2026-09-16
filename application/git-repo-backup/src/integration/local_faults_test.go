//go:build integration

// Local fault injection: capacity preflight, stale incomplete cleanup, and
// signal termination mid-run.
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLocalCapacityPreflightFails verifies an impossible minFreeBytes
// requirement stops the run before anything is written: no backup, no
// staging residue.
func TestLocalCapacityPreflightFails(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfigOpts(t, dir, runConfig{
		backupRoot: backupRoot, workspace: workspace, reposYAML: repos,
		maxBackups: 3, retentionOn: true, minFreeBytes: "900000000000000",
	})
	code, out := runBackup(t, bin, cfg)
	if code == 0 || !strings.Contains(out, "capacity") {
		t.Fatalf("expected capacity failure, got code=%d:\n%s", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(backupRoot, "backups")); len(entries) != 0 {
		t.Fatal("capacity failure must not publish backups")
	}
	if entries, _ := os.ReadDir(filepath.Join(backupRoot, ".staging")); len(entries) != 0 {
		t.Fatal("capacity failure must not leave staging residue")
	}
}

// TestLocalStaleIncompleteCleanup seeds over-age staging, trash, and
// backup-without-marker directories plus recent ones, runs once, and
// verifies exactly the over-age entries are cleaned while recent data and
// unrelated names survive.
func TestLocalStaleIncompleteCleanup(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	old := time.Now().UTC().Add(-72 * time.Hour).Format("20060102T150405Z")
	fresh := time.Now().UTC().Format("20060102T150405Z")
	for _, layout := range []struct{ base, id, extra string }{
		{".staging", old + "-staleowner", ""},
		{".staging", fresh + "-freshowner", ""},
		{".trash", old + "-staleowner", ""},
		{"backups", old, "no-marker"},
		{"backups", "unrelated-junk", "keep"},
	} {
		p := filepath.Join(backupRoot, layout.base, layout.id)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "sentinel"), []byte(layout.extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfig(t, dir, backupRoot, workspace, repos, 5)
	if code, out := runBackup(t, bin, cfg); code != 0 {
		t.Fatalf("run failed (%d):\n%s", code, out)
	}

	if _, err := os.Stat(filepath.Join(backupRoot, ".staging", old+"-staleowner")); !os.IsNotExist(err) {
		t.Fatal("over-age staging must be cleaned")
	}
	if _, err := os.Stat(filepath.Join(backupRoot, ".trash", old+"-staleowner")); !os.IsNotExist(err) {
		t.Fatal("over-age trash must be cleaned")
	}
	if _, err := os.Stat(filepath.Join(backupRoot, "backups", old)); !os.IsNotExist(err) {
		t.Fatal("over-age marker-less backup dir must be cleaned")
	}
	if _, err := os.Stat(filepath.Join(backupRoot, ".staging", fresh+"-freshowner")); err != nil {
		t.Fatal("recent staging must be kept for diagnosis")
	}
	if _, err := os.Stat(filepath.Join(backupRoot, "backups", "unrelated-junk")); err != nil {
		t.Fatal("unrelated names must never be touched")
	}
}

// TestLocalSIGTERMDuringRun interrupts a large clone: the process must exit
// with the SIGTERM code (143), publish nothing, and leave no success
// marker. The payload is sized so the clone is reliably still running when
// the signal fires.
func TestLocalSIGTERMDuringRun(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedIncompressibleRepo(t, dir, "big", 160)
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	repos := fmt.Sprintf("repositories:\n  - name: big\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfig(t, dir, backupRoot, workspace, repos, 3)

	cmd := exec.Command(bin, "run", "--config", cfg)
	stdout, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The clone of ~160 MiB over localhost SSH runs for seconds; signal
	// well inside it.
	time.Sleep(400 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process did not exit after SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 143 {
		t.Fatalf("expected exit code 143 after SIGTERM, got %d", code)
	}
	if entries, _ := os.ReadDir(filepath.Join(backupRoot, "backups")); len(entries) != 0 {
		t.Fatal("interrupted run must not publish a backup")
	}
	markerEverywhere(t, backupRoot, false)
}

// markerEverywhere asserts that no _SUCCESS file exists (want=false) under
// any staging or backup directory.
func markerEverywhere(t *testing.T, backupRoot string, want bool) {
	t.Helper()
	found := false
	for _, base := range []string{"backups", ".staging", ".trash"} {
		entries, err := os.ReadDir(filepath.Join(backupRoot, base))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(backupRoot, base, e.Name(), "_SUCCESS")); err == nil {
				found = true
			}
		}
	}
	if found != want {
		t.Fatalf("_SUCCESS presence = %v, want %v", found, want)
	}
}

// TestLocalSecondFailureNoOverwrite verifies a same-ID collision cannot
// overwrite an existing backup: the directories are pre-created and the run
// must fail instead.
func TestLocalSecondFailureNoOverwrite(t *testing.T) {
	canRunRooted(t)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	backupRoot := filepath.Join(dir, "backup")
	workspace := filepath.Join(dir, "workspace")

	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeRunConfig(t, dir, backupRoot, workspace, repos, 3)

	// Pre-create the backup directories the run is about to claim for the
	// current and next second; whichever it picks, the run must refuse to
	// overwrite instead of publishing.
	now := time.Now().UTC()
	hijacked := []string{
		filepath.Join(backupRoot, "backups", now.Format("20060102T150405Z")),
		filepath.Join(backupRoot, "backups", now.Add(time.Second).Format("20060102T150405Z")),
	}
	for _, d := range hijacked {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sentinel"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out := runBackup(t, bin, cfg)
	if code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("expected same-second collision failure, got code=%d:\n%s", code, out)
	}
	for _, d := range hijacked {
		if _, err := os.Stat(filepath.Join(d, "sentinel")); err != nil {
			t.Fatalf("existing backup directory %s must not be modified", d)
		}
	}
}
