package retention

import (
	"context"
	"log/slog"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

// Run applies the retention policy through the catalog. It runs only after
// the new backup committed successfully; any error here fails the Job while
// the published backup stays intact.
func Run(ctx context.Context, catalog storage.Catalog, currentID string, o Options, logger *slog.Logger) error {
	problems := runCompleteDeletions(ctx, catalog, currentID, o, logger)
	problems = append(problems, runIncompleteCleanup(ctx, catalog, o, logger)...)
	if len(problems) > 0 {
		return observability.WrapSafe(observability.CodeRetentionFailed,
			"retention reported problems", problems[0])
	}
	return nil
}

func runCompleteDeletions(ctx context.Context, catalog storage.Catalog, currentID string, o Options, logger *slog.Logger) []error {
	backups, err := catalog.ListBackups(ctx)
	if err != nil {
		return []error{err}
	}
	var problems []error
	for _, b := range backups {
		if b.Anomaly {
			logger.Error("backup has valid id but broken marker or metadata; skipping",
				"backupId", b.ID, "errorCode", observability.CodeRetentionFailed)
			problems = append(problems, observability.NewSafe(observability.CodeRetentionFailed,
				"backup "+b.ID+" has broken marker or metadata"))
		}
	}
	for _, b := range SelectDeletions(backups, currentID, o) {
		logger.Info("deleting backup by retention policy", "backupId", b.ID)
		if err := catalog.DeleteBackup(ctx, b); err != nil {
			logger.Error("backup deletion failed", "backupId", b.ID,
				"errorCode", observability.CodeOf(err))
			problems = append(problems, err)
		}
	}
	return problems
}

func runIncompleteCleanup(ctx context.Context, catalog storage.Catalog, o Options, logger *slog.Logger) []error {
	var problems []error
	stale, err := catalog.ListIncomplete(ctx)
	if err != nil {
		return []error{err}
	}
	for _, r := range stale {
		if !IncompleteEligible(r.StartedAt, o) {
			continue
		}
		logger.Info("cleaning incomplete run", "backupId", r.ID)
		if err := catalog.DeleteIncomplete(ctx, r); err != nil {
			logger.Error("incomplete cleanup failed", "backupId", r.ID,
				"errorCode", observability.CodeOf(err))
			problems = append(problems, err)
		}
	}
	cutoff := o.Now.Add(-o.MaxRunGrace)
	if o.IncompleteMaxAge > o.MaxRunGrace {
		cutoff = o.Now.Add(-o.IncompleteMaxAge)
	}
	if err := catalog.CleanupStaleUploads(ctx, cutoff); err != nil {
		logger.Error("stale upload cleanup failed", "errorCode", observability.CodeOf(err))
		problems = append(problems, err)
	}
	return problems
}
