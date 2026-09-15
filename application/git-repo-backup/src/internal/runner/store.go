package runner

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/gitmirror"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
	"git-repo-backup/internal/storage"
	localstorage "git-repo-backup/internal/storage/local"
	s3storage "git-repo-backup/internal/storage/s3"
	"git-repo-backup/internal/version"
)

func openStore(ctx context.Context, cfg *config.Config, logger *slog.Logger, ws *workspace, now func() time.Time) (*dependencies, storage.Catalog, func(), error) {
	switch cfg.Storage.Type {
	case "local":
		st, err := localstorage.New(cfg.Storage.Local.Root, cfg.Backup.MinFreeBytes, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		return &dependencies{cfg: cfg, logger: logger, store: st, ws: ws, now: now}, st,
			func() { _ = st.Close(context.Background()) }, nil
	default:
		st, err := s3storage.New(ctx, cfg.Storage.S3, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		staging := filepath.Join(ws.runDir, "staging")
		if err := safefs.EnsureDir(staging, 0o700); err != nil {
			return nil, nil, nil, observability.WrapSafe(observability.CodeStorageFailed,
				"prepare s3 staging directory", err)
		}
		st.SetStagingDir(staging)
		return &dependencies{cfg: cfg, logger: logger, store: st, ws: ws, now: now}, st,
			func() { _ = st.Close(context.Background()) }, nil
	}
}

func logStartup(logger *slog.Logger, cfg *config.Config, gitRunner *gitmirror.Runner, repoCount int) {
	gitVer, err := gitRunner.Versions(context.Background())
	if err != nil {
		gitVer = "unknown"
	}
	logger.Info("run starting", "event", "run_start",
		"backupVersion", version.Get(), "gitVersion", gitVer, "sshVersion", SSHVersion(),
		"compressionLevel", cfg.Backup.CompressionLevel,
		"cacheEnabled", cfg.Cache.Enabled,
		"retentionEnabled", cfg.Retention.Enabled,
		"repositoryCount", repoCount)
}
