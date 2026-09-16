// Package retention computes which backups are deletable (pure policy, no
// I/O) and orchestrates cleanup through the storage catalog.
package retention

import (
	"sort"
	"time"

	"git-repo-backup/internal/storage"
)

// Options is the effective retention policy for one run.
type Options struct {
	// Enabled toggles deletion of complete backups. Incomplete-run and
	// multipart cleanup always runs.
	Enabled bool
	// MaxBackups keeps the newest N complete backups; 0 disables.
	MaxBackups int
	// MaxAge deletes complete backups older than now-MaxAge; 0 disables.
	MaxAge time.Duration
	// IncompleteMaxAge is the minimum age before incomplete data may be
	// cleaned. It must already exceed the max run duration.
	IncompleteMaxAge time.Duration
	// MaxRunGrace is maxRunDuration plus the safety window; cleanup never
	// touches anything younger than this.
	MaxRunGrace time.Duration
	Now         time.Time
}

// SelectDeletions returns the complete backups to delete:
//
//  1. complete backups sorted by backup ID (UTC timestamp) descending;
//  2. candidates are those beyond the newest MaxBackups, older than
//     now-MaxAge, or both (union of the two rules);
//  3. the current run's backup and any backup with an ID greater than the
//     current ID are always protected, so a slower concurrent run can never
//     delete a newer backup published while it was still working.
//
// Anomalous entries are never deletable; the orchestrator reports them.
func SelectDeletions(backups []storage.Backup, currentID string, o Options) []storage.Backup {
	var complete []storage.Backup
	for _, b := range backups {
		if b.Complete && !b.Anomaly {
			complete = append(complete, b)
		}
	}
	sort.Slice(complete, func(i, j int) bool { return complete[i].ID > complete[j].ID })
	cutoff := time.Time{}
	if o.MaxAge > 0 {
		cutoff = o.Now.Add(-o.MaxAge)
	}
	keep := o.MaxBackups
	var deletions []storage.Backup
	// newer counts unprotected entries newer than the current candidate;
	// the current run's own backup (never deletable) occupies one of the
	// newest maxBackups slots and is therefore counted here as well.
	newer := 0
	for _, b := range complete {
		if currentID != "" && b.ID > currentID {
			continue // protected concurrent run; consumes no slot
		}
		if b.ID != currentID {
			byCount := o.Enabled && keep > 0 && newer >= keep
			byAge := o.Enabled && o.MaxAge > 0 && b.StartedAt.Before(cutoff)
			if byCount || byAge {
				deletions = append(deletions, b)
			}
		}
		newer++
	}
	return deletions
}

// IncompleteEligible reports whether an incomplete run is old enough to be
// cleaned: strictly older than both IncompleteMaxAge and the max-run
// duration grace window, and never for future timestamps.
func IncompleteEligible(startedAt time.Time, o Options) bool {
	if startedAt.IsZero() || startedAt.After(o.Now) {
		return false
	}
	minAge := o.IncompleteMaxAge
	if o.MaxRunGrace > minAge {
		minAge = o.MaxRunGrace
	}
	return o.Now.Sub(startedAt) > minAge
}
