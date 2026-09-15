package config

import (
	"fmt"

	"git-repo-backup/internal/safefs"
)

// Validate enforces every cross-field rule from the design document and
// parses duration strings once. Error messages never echo configuration
// values beyond field names.
func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", c.SchemaVersion)
	}
	if err := c.validatePaths(); err != nil {
		return err
	}
	switch c.Storage.Type {
	case "local":
	case "s3":
		if err := c.validateS3(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("storage.type must be local or s3")
	}
	if err := c.validateDurations(); err != nil {
		return err
	}
	if c.Backup.CompressionLevel < 1 || c.Backup.CompressionLevel > 9 {
		return fmt.Errorf("backup.compressionLevel must be between 1 and 9")
	}
	if c.Backup.MinFreeBytes == 0 {
		return fmt.Errorf("backup.minFreeBytes must be positive")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug, info, warn, error")
	}
	if c.Prepare.TargetUID <= 0 || c.Prepare.TargetGID <= 0 {
		return fmt.Errorf("prepare.targetUID and prepare.targetGID must be positive")
	}
	return nil
}

func (c *Config) validatePaths() error {
	checks := []struct{ name, path string }{
		{"repositoriesFile", c.RepositoriesFile},
		{"ssh.privateKeyFile", c.SSH.PrivateKeyFile},
		{"ssh.knownHostsFile", c.SSH.KnownHostsFile},
		{"workspace.root", c.Workspace.Root},
		{"prepare.inputPrivateKeyFile", c.Prepare.InputPrivateKeyFile},
		{"prepare.inputKnownHostsFile", c.Prepare.InputKnownHostsFile},
		{"prepare.sshDir", c.Prepare.SSHDir},
		{"prepare.tempDir", c.Prepare.TempDir},
	}
	if c.Storage.Type == "local" {
		checks = append(checks, struct{ name, path string }{"storage.local.root", c.Storage.Local.Root})
	}
	if c.Storage.Type == "s3" {
		checks = append(checks,
			struct{ name, path string }{"storage.s3.credentialsDir", c.Storage.S3.CredentialsDir},
			struct{ name, path string }{"storage.s3.webIdentityTokenDir", c.Storage.S3.WebIdentityTokenDir},
		)
		if c.Storage.S3.CABundleFile != "" {
			checks = append(checks, struct{ name, path string }{"storage.s3.caBundleFile", c.Storage.S3.CABundleFile})
		}
	}
	if c.Cache.Enabled {
		checks = append(checks, struct{ name, path string }{"cache.root", c.Cache.Root})
	}
	for _, ck := range checks {
		if err := safefs.ValidateAbsolutePath(ck.name, ck.path); err != nil {
			return err
		}
	}
	return c.validatePathContainment()
}

// validatePathContainment rejects backup/cache/workspace roots that contain
// each other. The single sanctioned exception is <local-root>/cache, which
// sits beside backups/ and .staging/ rather than inside archived data.
func (c *Config) validatePathContainment() error {
	local := c.Storage.Local.Root
	if c.Storage.Type == "local" {
		if err := checkNotNested("storage.local.root", local, "workspace.root", c.Workspace.Root); err != nil {
			return err
		}
		if c.Cache.Enabled {
			if err := checkCacheRoot(local, c.Cache.Root, c.Workspace.Root); err != nil {
				return err
			}
		}
		return nil
	}
	if c.Cache.Enabled {
		if err := checkNotNested("cache.root", c.Cache.Root, "workspace.root", c.Workspace.Root); err != nil {
			return err
		}
	}
	return nil
}

func checkCacheRoot(local, cache, workspace string) error {
	if cache == local || safefs.ContainsPath(local, cache) {
		if cache != local+"/cache" {
			return fmt.Errorf("cache.root inside storage.local.root must be exactly <local-root>/cache")
		}
		return nil
	}
	if err := checkNotNested("cache.root", cache, "storage.local.root", local); err != nil {
		return err
	}
	return checkNotNested("cache.root", cache, "workspace.root", workspace)
}

func checkNotNested(nameA, pathA, nameB, pathB string) error {
	if pathA == pathB {
		return fmt.Errorf("%s and %s must not be the same path", nameA, nameB)
	}
	if safefs.ContainsPath(pathA, pathB) {
		return fmt.Errorf("%s must not be inside %s", nameB, nameA)
	}
	if safefs.ContainsPath(pathB, pathA) {
		return fmt.Errorf("%s must not be inside %s", nameA, nameB)
	}
	return nil
}
