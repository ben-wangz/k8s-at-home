package config

// Fixed filesystem layout shared by the chart, container, and binary.
const (
	DefaultRepositoriesFile  = "/etc/git-repo-backup/repositories/repositories.yaml"
	DefaultSSHPrivateKeyFile = "/etc/git-repo-backup/ssh/id"
	DefaultSSHKnownHostsFile = "/etc/git-repo-backup/ssh/known_hosts"
	DefaultS3CredentialsDir  = "/etc/git-repo-backup/s3"
	DefaultCABundleFile      = "/etc/git-repo-backup/ca/ca.crt"
	DefaultWebIdentityDir    = "/etc/git-repo-backup/web-identity"
	DefaultLocalRoot         = "/backup"
	DefaultCacheRoot         = "/backup/cache"
	DefaultWorkspaceRoot     = "/workspace"
	DefaultPrepareSSHDir     = "/prepared-ssh"
	DefaultPrepareTempDir    = "/tmp"
	DefaultPrepareInputKey   = "/input/ssh/id"
	DefaultPrepareInputKnown = "/input/known-hosts/known_hosts"
	DefaultTargetUID         = 10001
	DefaultTargetGID         = 10001
)

// applyDefaults fills unset fields with the documented defaults. The chart
// renders every field explicitly; standalone configs may rely on these.
func (c *Config) applyDefaults() {
	if c.RepositoriesFile == "" {
		c.RepositoriesFile = DefaultRepositoriesFile
	}
	if c.SSH.PrivateKeyFile == "" {
		c.SSH.PrivateKeyFile = DefaultSSHPrivateKeyFile
	}
	if c.SSH.KnownHostsFile == "" {
		c.SSH.KnownHostsFile = DefaultSSHKnownHostsFile
	}
	if c.Storage.Local.Root == "" {
		c.Storage.Local.Root = DefaultLocalRoot
	}
	if c.Storage.S3.CredentialsDir == "" {
		c.Storage.S3.CredentialsDir = DefaultS3CredentialsDir
	}
	if c.Storage.S3.WebIdentityTokenDir == "" {
		c.Storage.S3.WebIdentityTokenDir = DefaultWebIdentityDir
	}
	if c.Workspace.Root == "" {
		c.Workspace.Root = DefaultWorkspaceRoot
	}
	if c.Cache.Root == "" {
		c.Cache.Root = DefaultCacheRoot
	}
	if c.Prepare.TargetUID == 0 {
		c.Prepare.TargetUID = DefaultTargetUID
	}
	if c.Prepare.TargetGID == 0 {
		c.Prepare.TargetGID = DefaultTargetGID
	}
	if c.Prepare.InputPrivateKeyFile == "" {
		c.Prepare.InputPrivateKeyFile = DefaultPrepareInputKey
	}
	if c.Prepare.InputKnownHostsFile == "" {
		c.Prepare.InputKnownHostsFile = DefaultPrepareInputKnown
	}
	if c.Prepare.SSHDir == "" {
		c.Prepare.SSHDir = DefaultPrepareSSHDir
	}
	if c.Prepare.TempDir == "" {
		c.Prepare.TempDir = DefaultPrepareTempDir
	}
	if c.Backup.CompressionLevel == 0 {
		c.Backup.CompressionLevel = 6
	}
	if c.Backup.MaxRunDuration == "" {
		c.Backup.MaxRunDuration = "5h30m"
	}
	if c.Backup.GitTimeout == "" {
		c.Backup.GitTimeout = "1h"
	}
	if c.Backup.MinFreeBytes == 0 {
		c.Backup.MinFreeBytes = 1 << 30
	}
	if c.Retention.MaxBackups == 0 && c.Retention.MaxAge == "" && c.Retention.IncompleteMaxAge == "" && !c.retentionPresent {
		c.Retention.Enabled = true
		c.Retention.MaxBackups = 30
		c.Retention.MaxAge = "720h"
		c.Retention.IncompleteMaxAge = "24h"
	}
	if c.Retention.IncompleteMaxAge == "" {
		c.Retention.IncompleteMaxAge = "24h"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}
