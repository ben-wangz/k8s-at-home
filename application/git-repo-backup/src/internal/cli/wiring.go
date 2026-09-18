package cli

import (
	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/prepare"
	"git-repo-backup/internal/version"
)

func buildVersion() string { return version.Get() }

func prepareRun(cfg *config.Config) error { return prepare.Run(cfg) }

func loadRepositories(cfg *config.Config) ([]config.Repository, error) {
	repos, err := config.LoadRepositories(cfg.RepositoriesFile)
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeInputInvalid, err.Error(), nil)
	}
	if err := config.ValidateSSHRequirement(repos, cfg.SSH.IsEnabled()); err != nil {
		return nil, observability.WrapSafe(observability.CodeInputInvalid, err.Error(), nil)
	}
	return repos, nil
}
