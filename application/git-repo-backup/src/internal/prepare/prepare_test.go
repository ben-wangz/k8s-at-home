package prepare

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"git-repo-backup/internal/config"
)

func testConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	inSSH := filepath.Join(dir, "input-ssh")
	inKnown := filepath.Join(dir, "input-known")
	prepared := filepath.Join(dir, "prepared-ssh")
	workspace := filepath.Join(dir, "workspace")
	backup := filepath.Join(dir, "backup")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{inSSH, inKnown, prepared, workspace, backup, tmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inSSH, "id"), []byte("KEY"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inKnown, "known_hosts"), []byte("host key"), 0o444); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SSH:       config.SSHConfig{HostKeyPolicy: "pinned"},
		Storage:   config.StorageConfig{Type: "local", Local: config.LocalStorage{Root: backup}},
		Workspace: config.WorkspaceConfig{Root: workspace},
		Prepare: config.PrepareConfig{
			TargetUID:           os.Getuid(),
			TargetGID:           os.Getgid(),
			InputPrivateKeyFile: filepath.Join(inSSH, "id"),
			InputKnownHostsFile: filepath.Join(inKnown, "known_hosts"),
			SSHDir:              prepared,
			TempDir:             tmp,
		},
	}
	return cfg, dir
}

func dirOwnerMode(t *testing.T, path string) (int, int, os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), info.Mode().Perm()
}

func TestPrepareSSHVolume(t *testing.T) {
	cfg, _ := testConfig(t)
	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	idPath := filepath.Join(cfg.Prepare.SSHDir, "id")
	knownPath := filepath.Join(cfg.Prepare.SSHDir, "known_hosts")
	idInfo, err := os.Lstat(idPath)
	if err != nil {
		t.Fatal(err)
	}
	if idInfo.Mode().Perm() != 0o400 || !idInfo.Mode().IsRegular() {
		t.Fatalf("prepared key mode wrong: %v", idInfo.Mode())
	}
	knownInfo, _ := os.Lstat(knownPath)
	if knownInfo.Mode().Perm() != 0o444 {
		t.Fatalf("prepared known_hosts mode wrong: %v", knownInfo.Mode())
	}
	uid, gid, mode := dirOwnerMode(t, cfg.Prepare.SSHDir)
	if uid != cfg.Prepare.TargetUID || gid != cfg.Prepare.TargetGID || mode != 0o700 {
		t.Fatalf("prepared dir ownership wrong: uid=%d gid=%d mode=%v", uid, gid, mode)
	}
	// Idempotent second run over an already-prepared volume.
	if err := Run(cfg); err != nil {
		t.Fatalf("second prepare must be a no-op: %v", err)
	}
}

func TestPrepareAcceptNewKnownHostsState(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.SSH.HostKeyPolicy = "accept-new"
	stateVolume := filepath.Join(dir, "known-hosts-state")
	if err := os.Mkdir(stateVolume, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Prepare.InputKnownHostsFile = ""
	cfg.Prepare.KnownHostsStateFile = filepath.Join(stateVolume, "state", "known_hosts")

	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Lstat(cfg.Prepare.KnownHostsStateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !stateInfo.Mode().IsRegular() || stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("accept-new state file mode wrong: %v", stateInfo.Mode())
	}
	if stateInfo.Size() != 0 {
		t.Fatal("accept-new state should start empty without a seed")
	}
	uid, gid, mode := dirOwnerMode(t, filepath.Dir(cfg.Prepare.KnownHostsStateFile))
	if uid != cfg.Prepare.TargetUID || gid != cfg.Prepare.TargetGID || mode != 0o700 {
		t.Fatalf("accept-new state directory ownership wrong: uid=%d gid=%d mode=%v", uid, gid, mode)
	}
	if err := Run(cfg); err != nil {
		t.Fatalf("second accept-new prepare must be a no-op: %v", err)
	}
}

func TestPrepareSkipsSSHWhenDisabled(t *testing.T) {
	cfg, _ := testConfig(t)
	enabled := false
	cfg.SSH.Enabled = &enabled
	if err := os.RemoveAll(cfg.Prepare.SSHDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(cfg.Prepare.InputPrivateKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(cfg.Prepare.InputKnownHostsFile)); err != nil {
		t.Fatal(err)
	}
	if err := Run(cfg); err != nil {
		t.Fatalf("HTTPS-only preparation must not require SSH inputs: %v", err)
	}
}

func TestPrepareVolumeRoots(t *testing.T) {
	cfg, _ := testConfig(t)
	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{cfg.Workspace.Root, cfg.Storage.Local.Root, cfg.Prepare.TempDir} {
		uid, gid, mode := dirOwnerMode(t, root)
		if uid != cfg.Prepare.TargetUID || gid != cfg.Prepare.TargetGID || mode != 0o700 {
			t.Fatalf("volume root %s not handed over: uid=%d gid=%d mode=%v", root, uid, gid, mode)
		}
	}
}

func TestPrepareCreatesLocalCacheSubdirectory(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.Cache = config.CacheConfig{
		Enabled: true,
		Root:    filepath.Join(cfg.Storage.Local.Root, "cache"),
	}

	if err := Run(cfg); err != nil {
		t.Fatalf("fresh local cache directory must be prepared: %v", err)
	}
	uid, gid, mode := dirOwnerMode(t, cfg.Cache.Root)
	if uid != cfg.Prepare.TargetUID || gid != cfg.Prepare.TargetGID || mode != 0o700 {
		t.Fatalf("cache directory ownership wrong: uid=%d gid=%d mode=%v", uid, gid, mode)
	}
}

func TestPrepareRejectsForeignNonEmptyVolume(t *testing.T) {
	cfg, _ := testConfig(t)
	// Another user's non-empty volume: simulate by placing a file and
	// pretending a different target uid is required.
	cfg.Prepare.TargetUID = cfg.Prepare.TargetUID + 1
	cfg.Prepare.TargetGID = cfg.Prepare.TargetGID + 1
	if err := os.WriteFile(filepath.Join(cfg.Workspace.Root, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(cfg); err == nil {
		t.Fatal("non-empty volume with wrong owner must be rejected")
	}
}

func TestPrepareRejectsEmptyKnownHosts(t *testing.T) {
	cfg, _ := testConfig(t)
	// Replace the read-only 0444 fixture with empty content.
	if err := os.Remove(cfg.Prepare.InputKnownHostsFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Prepare.InputKnownHostsFile, []byte("  \n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := Run(cfg); err == nil {
		t.Fatal("empty known_hosts must be rejected")
	}
}
