// Package config implements strict YAML decoding, defaults, and validation
// for the backup run configuration and the repository list.
package config

import "time"

// SchemaVersion is the only supported configuration schema version.
const SchemaVersion = 1

// Config is the decoded run configuration. Path and duration strings are
// validated during Load and exposed as parsed Dur* fields.
type Config struct {
	SchemaVersion    int             `yaml:"schemaVersion"`
	RepositoriesFile string          `yaml:"repositoriesFile"`
	SSH              SSHConfig       `yaml:"ssh"`
	Storage          StorageConfig   `yaml:"storage"`
	Workspace        WorkspaceConfig `yaml:"workspace"`
	Cache            CacheConfig     `yaml:"cache"`
	Prepare          PrepareConfig   `yaml:"prepare"`
	Backup           BackupConfig    `yaml:"backup"`
	Retention        RetentionConfig `yaml:"retention"`
	Log              LogConfig       `yaml:"log"`

	// Parsed durations, filled by Load.
	MaxRunDuration   time.Duration
	GitTimeout       time.Duration
	MaxAge           time.Duration // 0 disables the age rule
	IncompleteMaxAge time.Duration

	// retentionPresent records whether the retention section appeared in the
	// YAML document, so a fully absent section gets the default policy.
	retentionPresent bool
}

// SSHConfig controls the optional SSH material used by SSH repository URLs.
type SSHConfig struct {
	// Enabled is a pointer so standalone configurations that predate this
	// field retain the SSH default while chart values can explicitly disable it.
	Enabled        *bool  `yaml:"enabled"`
	HostKeyPolicy  string `yaml:"hostKeyPolicy"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
	KnownHostsFile string `yaml:"knownHostsFile"`
}

// IsEnabled reports whether SSH material is required for this configuration.
// An omitted field preserves the original SSH-enabled behavior.
func (c SSHConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveHostKeyPolicy returns the policy used by standalone configs that
// omit the field. New deployments use OpenSSH's TOFU behavior by default.
func (c SSHConfig) EffectiveHostKeyPolicy() string {
	if c.HostKeyPolicy == "" {
		return DefaultSSHHostKeyPolicy
	}
	return c.HostKeyPolicy
}

// StorageConfig selects the local or S3 backend.
type StorageConfig struct {
	Type  string       `yaml:"type"`
	Local LocalStorage `yaml:"local"`
	S3    S3Storage    `yaml:"s3"`
}

// LocalStorage is the backup volume root for the local backend.
type LocalStorage struct {
	Root string `yaml:"root"`
}

// S3Storage configures the S3-compatible destination.
type S3Storage struct {
	Endpoint            string `yaml:"endpoint"`
	Region              string `yaml:"region"`
	Bucket              string `yaml:"bucket"`
	Prefix              string `yaml:"prefix"`
	ForcePathStyle      bool   `yaml:"forcePathStyle"`
	ServerSideEncrypt   string `yaml:"serverSideEncryption"`
	KMSKeyID            string `yaml:"kmsKeyId"`
	CredentialsMode     string `yaml:"credentialsMode"`
	CredentialsDir      string `yaml:"credentialsDir"`
	RoleARN             string `yaml:"roleArn"`
	CABundleFile        string `yaml:"caBundleFile"`
	WebIdentityTokenDir string `yaml:"webIdentityTokenDir"`
}

// WorkspaceConfig is the scratch volume for mirrors and S3 staging.
type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

// CacheConfig controls the optional persistent mirror cache.
type CacheConfig struct {
	Enabled bool   `yaml:"enabled"`
	Root    string `yaml:"root"`
}

// PrepareConfig is consumed only by the prepare subcommand and the chart.
type PrepareConfig struct {
	TargetUID           int    `yaml:"targetUID"`
	TargetGID           int    `yaml:"targetGID"`
	InputPrivateKeyFile string `yaml:"inputPrivateKeyFile"`
	InputKnownHostsFile string `yaml:"inputKnownHostsFile"`
	KnownHostsStateFile string `yaml:"knownHostsStateFile"`
	SSHDir              string `yaml:"sshDir"`
	TempDir             string `yaml:"tempDir"`
}

// BackupConfig holds run-wide backup tuning values.
type BackupConfig struct {
	CompressionLevel int    `yaml:"compressionLevel"`
	MaxRunDuration   string `yaml:"maxRunDuration"`
	GitTimeout       string `yaml:"gitTimeout"`
	MinFreeBytes     uint64 `yaml:"minFreeBytes"`
}

// RetentionConfig holds retention policy values. MaxAge "" and MaxBackups 0
// each disable that rule.
type RetentionConfig struct {
	Enabled          bool   `yaml:"enabled"`
	MaxBackups       int    `yaml:"maxBackups"`
	MaxAge           string `yaml:"maxAge"`
	IncompleteMaxAge string `yaml:"incompleteMaxAge"`
}

// LogConfig selects the log level.
type LogConfig struct {
	Level string `yaml:"level"`
}

// Repository is one entry of the repositories file.
type Repository struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// RepositoriesFile is the fixed schema of repositories.yaml.
type RepositoriesFile struct {
	Repositories []Repository `yaml:"repositories"`
}
