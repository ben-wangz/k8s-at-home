package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
schemaVersion: 1
storage:
  type: local
  local:
    root: /backup
`

func TestLoadStrictDecoding(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown field", minimalConfig + "\nunknownKey: 1\n"},
		{"duplicate key", minimalConfig + "\nworkspace:\n  root: /a\nworkspace:\n  root: /b\n"},
		{"second document", minimalConfig + "\n---\nschemaVersion: 1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, "config.yaml", tc.body)); err == nil {
				t.Fatal("expected strict rejection")
			}
		})
	}
}

func TestLoadRejectsSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.yaml")
	big := make([]byte, MaxConfigSize+1)
	for i := range big {
		big[i] = '#'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, "config.yaml", "schemaVersion: 1\nstorage:\n  type: local\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepositoriesFile != DefaultRepositoriesFile {
		t.Errorf("repositoriesFile default not applied")
	}
	if cfg.SSH.PrivateKeyFile != DefaultSSHPrivateKeyFile {
		t.Errorf("ssh key default not applied")
	}
	if cfg.SSH.EffectiveHostKeyPolicy() != DefaultSSHHostKeyPolicy {
		t.Errorf("ssh host key policy default not applied")
	}
	if cfg.SSH.KnownHostsFile != DefaultSSHStateKnownHosts {
		t.Errorf("accept-new known_hosts path default not applied")
	}
	if cfg.Prepare.KnownHostsStateFile != DefaultPrepareKnownState {
		t.Errorf("known_hosts state path default not applied")
	}
	if !cfg.SSH.IsEnabled() {
		t.Errorf("ssh enabled default not applied")
	}
	if cfg.Workspace.Root != DefaultWorkspaceRoot {
		t.Errorf("workspace default not applied")
	}
	if cfg.Backup.CompressionLevel != 6 {
		t.Errorf("compression default not applied")
	}
	if cfg.MaxRunDuration == 0 || cfg.GitTimeout == 0 || cfg.IncompleteMaxAge == 0 {
		t.Errorf("duration defaults not parsed")
	}
}

func TestLoadRejectsUnknownSchema(t *testing.T) {
	if _, err := Load(writeTemp(t, "c.yaml", "schemaVersion: 2\nstorage:\n  type: local\n")); err == nil {
		t.Fatal("expected schema rejection")
	}
}

func TestLoadAllowsSSHDisabledWithoutSSHPaths(t *testing.T) {
	configPath := writeTemp(t, "config.yaml", `schemaVersion: 1
ssh:
  enabled: false
  privateKeyFile: relative/key
  knownHostsFile: relative/known_hosts
prepare:
  inputPrivateKeyFile: relative/input-key
  inputKnownHostsFile: relative/input-known-hosts
  sshDir: relative/prepared-ssh
storage:
  type: local
  local:
    root: /backup
`)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSH.IsEnabled() {
		t.Fatal("ssh.enabled=false must remain disabled")
	}
}

func TestValidateRules(t *testing.T) {
	base := func(mutate func(c *Config)) *Config {
		cfg := &Config{
			SchemaVersion:    1,
			RepositoriesFile: "/etc/git-repo-backup/repositories/repositories.yaml",
			SSH:              SSHConfig{HostKeyPolicy: "pinned", PrivateKeyFile: "/etc/x/id", KnownHostsFile: "/etc/x/kh"},
			Workspace:        WorkspaceConfig{Root: "/workspace"},
			Prepare: PrepareConfig{TargetUID: 10001, TargetGID: 10001,
				InputPrivateKeyFile: "/input/ssh/id", InputKnownHostsFile: "/input/known-hosts/known_hosts",
				SSHDir: "/prepared-ssh", TempDir: "/tmp"},
			Backup:           BackupConfig{CompressionLevel: 6, MaxRunDuration: "1h", GitTimeout: "30m", MinFreeBytes: 1},
			Retention:        RetentionConfig{Enabled: true, MaxBackups: 5, IncompleteMaxAge: "2h"},
			Log:              LogConfig{Level: "info"},
			Storage:          StorageConfig{Type: "local", Local: LocalStorage{Root: "/backup"}},
			retentionPresent: true,
		}
		mutate(cfg)
		return cfg
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"relative path", func(c *Config) { c.SSH.PrivateKeyFile = "relative/id" }},
		{"root path", func(c *Config) { c.Workspace.Root = "/" }},
		{"dotdot path", func(c *Config) { c.Workspace.Root = "/a/../b" }},
		{"control char path", func(c *Config) { c.Storage.Local.Root = "/ba\x01ck" }},
		{"workspace inside backup", func(c *Config) { c.Workspace.Root = "/backup/ws" }},
		{"backup inside workspace", func(c *Config) { c.Storage.Local.Root = "/workspace/backup" }},
		{"cache deep inside backup", func(c *Config) { c.Cache = CacheConfig{Enabled: true, Root: "/backup/cache/deep"} }},
		{"cache equals backup", func(c *Config) { c.Cache = CacheConfig{Enabled: true, Root: "/backup"} }},
		{"cache ok sibling", func(c *Config) { c.Cache = CacheConfig{Enabled: true, Root: "/backup/cache"} }},
		{"cache inside workspace", func(c *Config) { c.Cache = CacheConfig{Enabled: true, Root: "/workspace/cache"} }},
		{"bad storage type", func(c *Config) { c.Storage.Type = "ftp" }},
		{"compression low", func(c *Config) { c.Backup.CompressionLevel = 0 }},
		{"compression high", func(c *Config) { c.Backup.CompressionLevel = 10 }},
		{"duration days", func(c *Config) { c.Backup.MaxRunDuration = "30d" }},
		{"negative duration", func(c *Config) { c.Backup.GitTimeout = "-5m" }},
		{"log level", func(c *Config) { c.Log.Level = "trace" }},
		{"retention disabled both", func(c *Config) { c.Retention = RetentionConfig{Enabled: true, IncompleteMaxAge: "2h"} }},
		{"incomplete too small", func(c *Config) { c.Retention.IncompleteMaxAge = "1h" }},
		{"negative maxbackups", func(c *Config) { c.Retention.MaxBackups = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.mutate).Validate()
			expectErr := !strings.Contains(tc.name, "ok ")
			if expectErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !expectErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateSSHHostKeyPolicies(t *testing.T) {
	base := func(policy string) *Config {
		enabled := true
		return &Config{
			SchemaVersion:    1,
			RepositoriesFile: "/etc/git-repo-backup/repositories/repositories.yaml",
			SSH:              SSHConfig{Enabled: &enabled, HostKeyPolicy: policy, PrivateKeyFile: "/etc/x/id", KnownHostsFile: "/etc/x/kh"},
			Workspace:        WorkspaceConfig{Root: "/workspace"},
			Prepare: PrepareConfig{TargetUID: 10001, TargetGID: 10001,
				InputPrivateKeyFile: "/input/ssh/id", InputKnownHostsFile: "/input/known-hosts/known_hosts",
				KnownHostsStateFile: "/known-hosts-state/state/known_hosts", SSHDir: "/prepared-ssh", TempDir: "/tmp"},
			Backup:           BackupConfig{CompressionLevel: 6, MaxRunDuration: "1h", GitTimeout: "30m", MinFreeBytes: 1},
			Retention:        RetentionConfig{Enabled: true, MaxBackups: 5, IncompleteMaxAge: "2h"},
			Log:              LogConfig{Level: "info"},
			Storage:          StorageConfig{Type: "local", Local: LocalStorage{Root: "/backup"}},
			retentionPresent: true,
		}
	}

	t.Run("accept-new", func(t *testing.T) {
		cfg := base("accept-new")
		cfg.Prepare.InputKnownHostsFile = ""
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("pinned requires seed", func(t *testing.T) {
		cfg := base("pinned")
		cfg.Prepare.InputKnownHostsFile = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("pinned policy must require a known_hosts seed")
		}
	})
	t.Run("none does not require state", func(t *testing.T) {
		cfg := base("none")
		cfg.Prepare.InputKnownHostsFile = ""
		cfg.Prepare.KnownHostsStateFile = ""
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown policy", func(t *testing.T) {
		if err := base("unsafe").Validate(); err == nil {
			t.Fatal("unknown host key policy must be rejected")
		}
	})
}
