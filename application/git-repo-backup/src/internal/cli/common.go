package cli

import (
	"flag"
	"fmt"
	"os"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
)

// parseConfigFlag handles the shared --config flag.
func parseConfigFlag(name string, args []string) (string, int, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return "", exitInput, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "git-repo-backup: unexpected argument %q\n", fs.Arg(0))
		return "", exitInput, false
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "git-repo-backup: --config is required")
		return "", exitInput, false
	}
	return *configPath, exitOK, true
}

// loadConfig reads and validates the configuration file. Errors are
// classified as configuration errors; their messages name fields only and
// never echo values.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeConfigInvalid, err.Error(), nil)
	}
	return cfg, nil
}

// validateCommand decodes and validates the configuration and every local
// reference file. It never connects to a remote, never creates locks or
// objects, and does not prove remote availability.
func validateCommand(args []string) int {
	path, code, ok := parseConfigFlag("validate", args)
	if !ok {
		return code
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitInput
	}
	checks := []struct{ name, path string }{}
	if cfg.SSH.IsEnabled() {
		checks = append(checks, struct{ name, path string }{"ssh.privateKeyFile", cfg.SSH.PrivateKeyFile})
		switch cfg.SSH.EffectiveHostKeyPolicy() {
		case "pinned":
			checks = append(checks, struct{ name, path string }{"ssh.knownHostsFile", cfg.SSH.KnownHostsFile})
		case "accept-new":
			if cfg.Prepare.InputKnownHostsFile != "" {
				checks = append(checks, struct{ name, path string }{"prepare.inputKnownHostsFile", cfg.Prepare.InputKnownHostsFile})
			}
		}
	}
	if cfg.Storage.Type == "s3" && cfg.Storage.S3.CABundleFile != "" {
		checks = append(checks, struct{ name, path string }{"storage.s3.caBundleFile", cfg.Storage.S3.CABundleFile})
	}
	for _, c := range checks {
		if err := config.FileExists(c.path); err != nil {
			fmt.Fprintf(os.Stderr, "git-repo-backup: %s: %s\n", c.name, err)
			return exitInput
		}
	}
	if _, err := loadRepositories(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitInput
	}
	fmt.Println("configuration valid")
	return exitOK
}

// versionCommand prints the build version.
func versionCommand(_ []string) int {
	fmt.Printf("git-repo-backup %s\n", buildVersion())
	return exitOK
}

// prepareCommand initializes mounted volumes and the SSH material as root.
func prepareCommand(args []string) int {
	path, code, ok := parseConfigFlag("prepare", args)
	if !ok {
		return code
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitInput
	}
	if err := prepareRun(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitFailure
	}
	return exitOK
}
