package runner

import (
	"context"
	"log/slog"
	"os"
	"time"

	"git-repo-backup/internal/archive"
	"git-repo-backup/internal/config"
	"git-repo-backup/internal/gitmirror"
	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/storage"
)

// backupRepository runs the per-repository pipeline: mirror (clone or
// cached fetch, HEAD sync, fsck), ref counting, archive creation, and
// hand-off to the store. The first error stops the whole run.
func backupRepository(ctx context.Context, deps *dependencies, repo config.Repository) (manifest.RepositoryEntry, error) {
	logger := deps.logger.With("repository", repo.Name)
	startedAt := deps.now()
	entry := manifest.RepositoryEntry{Name: repo.Name, Archive: manifest.ArchivePath(repo.Name), StartedAt: startedAt}

	logger.Info("repository backup started", "phase", "mirror")
	mirrorPath, ephemeral, err := deps.cache.PrepareMirror(ctx, deps.git, repo.Name, repo.URL, deps.ws.mirrorsDir())
	if err != nil {
		return entry, err
	}
	if ephemeral {
		defer os.RemoveAll(mirrorPath)
	}
	logger.Info("mirror ready", "phase", "mirror")

	refCount, err := deps.git.CountRefs(ctx, mirrorPath)
	if err != nil {
		return entry, err
	}
	entry.RefCount = refCount
	head, err := deps.git.LocalHead(ctx, mirrorPath)
	if err != nil {
		return entry, err
	}
	if head.Symbolic {
		entry.HeadRef = head.Ref
	} else if head.OID != "" {
		entry.HeadOID = head.OID
	}

	logger.Info("archiving mirror", "phase", "archive")
	info, err := deps.git.ReadConfigInfo(ctx, mirrorPath)
	if err != nil {
		return entry, err
	}
	info.RemoteURL = gitmirror.PlaceholderRemoteURL
	dest := deps.store.ArchivePath(repo.Name)
	result, err := archive.Create(dest, mirrorPath, repo.Name, deps.cfg.Backup.CompressionLevel, gitmirror.RenderBareConfig(info))
	if err != nil {
		return entry, err
	}
	entry.SHA256 = result.SHA256
	entry.SizeBytes = result.Size
	logger.Info("archive created", "phase", "archive",
		"sizeBytes", result.Size, "sha256", result.SHA256)

	artifact := storage.Artifact{
		Name: repo.Name, Path: dest, SHA256: result.SHA256, Size: result.Size, RefCount: refCount,
	}
	logger.Info("storing archive", "phase", "store")
	if err := deps.store.PutArchive(ctx, artifact); err != nil {
		return entry, err
	}
	if deps.cfg.Storage.Type == "s3" {
		// The upload is confirmed; free the local staging file.
		if err := os.Remove(dest); err != nil {
			return entry, err
		}
	}
	entry.CompletedAt = deps.now()
	logger.Info("repository backup completed", "phase", "repository_done",
		"durationMs", entry.CompletedAt.Sub(startedAt).Milliseconds())
	return entry, nil
}

// deps bundles the run's collaborators for the per-repository pipeline.
type dependencies struct {
	cfg    *config.Config
	logger *slog.Logger
	store  storage.RunStore
	cache  *gitmirror.Cache
	git    *gitmirror.Runner
	ws     *workspace
	now    func() time.Time
}
