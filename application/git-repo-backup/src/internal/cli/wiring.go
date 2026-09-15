package cli

import (
	"git-repo-backup/internal/config"
	"git-repo-backup/internal/prepare"
	"git-repo-backup/internal/version"
)

func buildVersion() string { return version.Get() }

func prepareRun(cfg *config.Config) error { return prepare.Run(cfg) }

func loadRepositories(cfg *config.Config) ([]config.Repository, error) {
	return config.LoadRepositories(cfg.RepositoriesFile)
}
