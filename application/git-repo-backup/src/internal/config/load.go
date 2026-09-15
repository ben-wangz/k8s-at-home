package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Size limits bound how much untrusted YAML is read before parsing.
const (
	MaxConfigSize       = 1 << 20  // 1 MiB
	MaxRepositoriesSize = 16 << 20 // 16 MiB
)

// rawConfig mirrors Config with a pointer retention field so section
// presence can be detected while unknown fields stay strictly rejected.
type rawConfig struct {
	SchemaVersion    int              `yaml:"schemaVersion"`
	RepositoriesFile string           `yaml:"repositoriesFile"`
	SSH              SSHConfig        `yaml:"ssh"`
	Storage          StorageConfig    `yaml:"storage"`
	Workspace        WorkspaceConfig  `yaml:"workspace"`
	Cache            CacheConfig      `yaml:"cache"`
	Prepare          PrepareConfig    `yaml:"prepare"`
	Backup           BackupConfig     `yaml:"backup"`
	Retention        *RetentionConfig `yaml:"retention"`
	Log              LogConfig        `yaml:"log"`
}

// strictDecode parses exactly one YAML document into out, rejecting unknown
// fields, duplicate keys (rejected by yaml.v3 itself), trailing documents,
// and files above the size limit.
func strictDecode(path string, data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: unexpected extra YAML document", path)
	}
	return nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds size limit of %d bytes", path, limit)
	}
	return data, nil
}

// Load reads, decodes, and validates the run configuration.
func Load(path string) (*Config, error) {
	data, err := readLimited(path, MaxConfigSize)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw rawConfig
	if err := strictDecode(path, data, &raw); err != nil {
		return nil, err
	}
	cfg := &Config{
		SchemaVersion:    raw.SchemaVersion,
		RepositoriesFile: raw.RepositoriesFile,
		SSH:              raw.SSH,
		Storage:          raw.Storage,
		Workspace:        raw.Workspace,
		Cache:            raw.Cache,
		Prepare:          raw.Prepare,
		Backup:           raw.Backup,
		Log:              raw.Log,
		retentionPresent: raw.Retention != nil,
	}
	if raw.Retention != nil {
		cfg.Retention = *raw.Retention
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
