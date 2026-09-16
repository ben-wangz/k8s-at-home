package retention

import (
	"testing"
	"time"

	"git-repo-backup/internal/storage"
)

var base = Options{
	Enabled:          true,
	MaxBackups:       3,
	MaxAge:           24 * time.Hour,
	IncompleteMaxAge: 48 * time.Hour,
	MaxRunGrace:      1 * time.Hour,
	Now:              time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
}

func mk(id string, age time.Duration) storage.Backup {
	return storage.Backup{ID: id, Complete: true, StartedAt: base.Now.Add(-age)}
}

func ids(backups []storage.Backup) []string {
	var out []string
	for _, b := range backups {
		out = append(out, b.ID)
	}
	return out
}

func TestSelectDeletionsByCount(t *testing.T) {
	// Five complete backups, keep the newest three. maxAge is far away.
	o := base
	o.MaxAge = 0
	list := []storage.Backup{
		mk("20260914T110000Z", 1*time.Hour),
		mk("20260913T110000Z", 25*time.Hour),
		mk("20260912T110000Z", 49*time.Hour),
		mk("20260911T110000Z", 73*time.Hour),
		mk("20260910T110000Z", 97*time.Hour),
	}
	got := ids(SelectDeletions(list, "", o))
	if len(got) != 2 || got[0] != "20260911T110000Z" || got[1] != "20260910T110000Z" {
		t.Fatalf("unexpected deletions: %v", got)
	}
}

func TestSelectDeletionsAgeUnion(t *testing.T) {
	// Keep 5 by count, but delete anything older than 24h regardless.
	o := base
	o.MaxBackups = 5
	list := []storage.Backup{
		mk("20260914T110000Z", 1*time.Hour),
		mk("20260914T100000Z", 2*time.Hour),
		mk("20260912T110000Z", 49*time.Hour), // old
	}
	got := ids(SelectDeletions(list, "", o))
	if len(got) != 1 || got[0] != "20260912T110000Z" {
		t.Fatalf("unexpected deletions: %v", got)
	}
}

func TestSelectDeletionsProtectsCurrentAndNewer(t *testing.T) {
	o := base
	o.MaxBackups = 1
	list := []storage.Backup{
		mk("20260914T120000Z", 0), // a concurrent run with a greater ID
		mk("20260914T110000Z", 1*time.Hour),
		mk("20260910T110000Z", 97*time.Hour),
	}
	got := ids(SelectDeletions(list, "20260914T113000Z", o))
	for _, id := range got {
		if id == "20260914T120000Z" {
			t.Fatal("must never delete a backup newer than the current run")
		}
		if id == "20260914T110000Z" {
			t.Fatal("the newest unprotected backup is still within maxBackups")
		}
	}
	if len(got) != 1 || got[0] != "20260910T110000Z" {
		t.Fatalf("expected only the oldest to go, got %v", got)
	}
	// The current backup itself is protected even when old.
	list = []storage.Backup{mk("20260910T110000Z", 97*time.Hour)}
	if got := SelectDeletions(list, "20260910T110000Z", o); len(got) != 0 {
		t.Fatalf("current backup must be protected: %v", ids(got))
	}
}

func TestSelectDeletionsByCountWithCurrentInList(t *testing.T) {
	// The current run's backup occupies one of the newest keep slots.
	o := base
	o.MaxAge = 0
	o.MaxBackups = 2
	current := "20260914T120000Z"
	list := []storage.Backup{
		mk(current, 0),
		mk("20260914T110000Z", 1*time.Hour),
		mk("20260914T100000Z", 2*time.Hour),
		mk("20260914T090000Z", 3*time.Hour),
	}
	got := ids(SelectDeletions(list, current, o))
	if len(got) != 2 || got[0] != "20260914T100000Z" || got[1] != "20260914T090000Z" {
		t.Fatalf("current must consume a keep slot; deletions: %v", got)
	}
}

func TestSelectDeletionsSkipsAnomaliesAndIncomplete(t *testing.T) {
	o := base
	o.MaxBackups = 0
	list := []storage.Backup{
		{ID: "20260910T110000Z", Complete: true, Anomaly: true, StartedAt: base.Now.Add(-97 * time.Hour)},
		{ID: "20260909T110000Z", Complete: false, StartedAt: base.Now.Add(-120 * time.Hour)},
	}
	if got := SelectDeletions(list, "", o); len(got) != 0 {
		t.Fatalf("anomalies and incomplete runs are never deletion candidates: %v", ids(got))
	}
}

func TestSelectDeletionsDisabled(t *testing.T) {
	o := base
	o.Enabled = false
	list := []storage.Backup{mk("20260910T110000Z", 97*time.Hour)}
	if got := SelectDeletions(list, "", o); len(got) != 0 {
		t.Fatalf("disabled retention must not delete: %v", ids(got))
	}
}

func TestIncompleteEligible(t *testing.T) {
	o := base // grace 1h, incomplete 48h -> effective min age 48h
	if IncompleteEligible(base.Now.Add(-47*time.Hour), o) {
		t.Fatal("younger than incompleteMaxAge must be kept")
	}
	if !IncompleteEligible(base.Now.Add(-49*time.Hour), o) {
		t.Fatal("older than incompleteMaxAge must be eligible")
	}
	if IncompleteEligible(base.Now.Add(time.Hour), o) {
		t.Fatal("future timestamps are never eligible")
	}
	o2 := o
	o2.MaxRunGrace = 100 * time.Hour // max run duration dominates
	if IncompleteEligible(base.Now.Add(-49*time.Hour), o2) {
		t.Fatal("younger than the max-run grace window must be kept")
	}
}
