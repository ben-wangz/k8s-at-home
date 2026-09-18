package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/gitmirror"
	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/retention"
	"git-repo-backup/internal/storage"
	"git-repo-backup/internal/version"
)

// cleanupBudget bounds abort and lock-release work after a failure.
const cleanupBudget = 30 * time.Second

// graceWindow is the safety margin added to maxRunDuration for cleanup
// eligibility decisions.
const graceWindow = 10 * time.Minute

// Options configures one run.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	Repos  []config.Repository
	PodUID string
	Now    func() time.Time
}

// Result reports what the run accomplished.
type Result struct {
	BackupID  string
	Published bool
}

// Run executes the whole backup state machine:
// Validate -> Begin -> [Mirror -> Archive -> Store]* -> Metadata ->
// Commit -> Retention -> Summary. The first repository error aborts with no
// marker; retention failures keep the published backup and still fail.
func Run(ctx context.Context, opts Options) (Result, error) {
	cfg := opts.Config
	now := opts.Now()
	backupID := storage.FormatBackupID(now)
	owner := NewOwnerUUID()
	logger := opts.Logger.With("backupId", backupID, "backend", cfg.Storage.Type)
	result := Result{BackupID: backupID}
	if err := config.ValidateSSHRequirement(opts.Repos, cfg.SSH.IsEnabled()); err != nil {
		return result, observability.WrapSafe(observability.CodeInputInvalid, err.Error(), nil)
	}

	runCtx, cancel := context.WithTimeout(ctx, cfg.MaxRunDuration)
	defer cancel()

	if err := preflight(cfg); err != nil {
		return result, err
	}
	ws, err := openWorkspace(cfg.Workspace.Root, cfg.Backup.MinFreeBytes)
	if err != nil {
		return result, err
	}
	defer ws.close()
	grace := cfg.MaxRunDuration + graceWindow
	ws.cleanupStale(now, cfg.IncompleteMaxAge, grace)
	if err := ws.beginRun(backupID, opts.PodUID); err != nil {
		return result, err
	}

	var cache *gitmirror.Cache
	if cfg.Cache.Enabled {
		cache, err = gitmirror.OpenCache(cfg.Cache.Root)
		if err != nil {
			return result, err
		}
		defer cache.Close()
		cache.CleanupTemp(now, maxDuration(cfg.IncompleteMaxAge, grace))
	}

	gitRunner, err := gitmirror.NewRunner("/usr/bin/git", filepath.Join(ws.runDir, ".git-home"), cfg.GitTimeout,
		gitmirror.SSHOptions{
			PrivateKeyFile: cfg.SSH.PrivateKeyFile,
			KnownHostsFile: cfg.SSH.KnownHostsFile,
			HostKeyPolicy:  cfg.SSH.EffectiveHostKeyPolicy(),
		})
	if err != nil {
		return result, err
	}
	deps, catalog, closeStore, err := openStore(runCtx, cfg, logger, ws, opts.Now)
	if err != nil {
		return result, err
	}
	defer closeStore()
	deps.cache = cache
	deps.git = gitRunner

	logStartup(logger, cfg, gitRunner, len(opts.Repos))

	committed := false
	defer func() {
		if committed {
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer ccancel()
		if err := deps.store.Abort(cctx); err != nil {
			logger.Warn("abort cleanup incomplete", "errorCode", observability.CodeOf(err))
		}
	}()

	if err := deps.store.Begin(runCtx, storage.RunIdentity{
		BackupID: backupID, OwnerUUID: owner, StartedAt: now,
	}); err != nil {
		return result, err
	}

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		BackupID:      backupID,
		StartedAt:     now,
		ToolVersions:  manifest.ToolVersions{Backup: version.Get()},
	}
	if m.ToolVersions.Git, err = gitRunner.Versions(runCtx); err != nil {
		return result, err
	}
	for _, repo := range opts.Repos {
		if err := runCtx.Err(); err != nil {
			return result, err
		}
		entry, err := backupRepository(runCtx, deps, repo)
		if err != nil {
			logger.Error("repository backup failed", "repository", repo.Name,
				"phase", "repository_failed", "errorCode", observability.CodeOf(err))
			return result, err
		}
		m.Repositories = append(m.Repositories, entry)
	}

	if err := runCtx.Err(); err != nil {
		return result, err
	}
	m.CompletedAt = opts.Now()
	m.SortRepositories()
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return result, observability.WrapSafe(observability.CodeStorageFailed, "encode manifest", err)
	}
	checksumsBytes, err := manifest.BuildChecksums(manifestBytes, m)
	if err != nil {
		return result, err
	}
	if err := deps.store.PutMetadata(runCtx, manifestBytes, checksumsBytes); err != nil {
		return result, err
	}

	marker := manifest.BuildMarker(backupID, manifestBytes, checksumsBytes)
	if err := commit(runCtx, deps.store, marker, logger); err != nil {
		var ce *storage.CommitError
		if errors.As(err, &ce) &&
			(ce.State == storage.PublishPublished || ce.State == storage.PublishPublishedDurabilityUncertain) {
			committed = true
			result.Published = true
		}
		return result, err
	}
	committed = true
	result.Published = true
	logger.Info("backup published", "phase", "commit", "published", true)

	if err := runCtx.Err(); err != nil {
		return result, err
	}
	retOpts := retention.Options{
		Enabled:          cfg.Retention.Enabled,
		MaxBackups:       cfg.Retention.MaxBackups,
		MaxAge:           cfg.MaxAge,
		IncompleteMaxAge: cfg.IncompleteMaxAge,
		MaxRunGrace:      grace,
		Now:              opts.Now(),
	}
	if err := retention.Run(runCtx, catalog, backupID, retOpts, logger); err != nil {
		logger.Error("retention failed after publication", "published", true,
			"phase", "retention_failed", "errorCode", observability.CodeOf(err))
		return result, err
	}
	logger.Info("run completed", "event", "run_completed", "published", true,
		"repositories", len(m.Repositories),
		"durationMs", opts.Now().Sub(now).Milliseconds())
	return result, nil
}

// commit wraps the store commit and maps publish states into logs.
func commit(ctx context.Context, store storage.RunStore, marker manifest.SuccessMarker, logger *slog.Logger) error {
	err := store.Commit(ctx, marker)
	if err == nil {
		return nil
	}
	var ce *storage.CommitError
	published := false
	uncertain := false
	if errors.As(err, &ce) {
		published = ce.State == storage.PublishPublished || ce.State == storage.PublishPublishedDurabilityUncertain
		uncertain = ce.State == storage.PublishPublishedDurabilityUncertain
	}
	logger.Error("commit failed", "published", published,
		"durabilityUncertain", uncertain, "errorCode", observability.CodeOf(err))
	return err
}
