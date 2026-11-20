package app

import (
	"context"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticEventRetentionStartsANewCutoffForEachRound(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	prefix := diagnostics.Prefix + "events/"
	firstStart := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	firstCutoff := firstStart.Add(-diagnostics.Retention)
	olderPartition := firstCutoff.Add(-24 * time.Hour).Format("20060102")
	cutoffPartition := firstCutoff.Format("20060102")
	for _, date := range []string{olderPartition, cutoffPartition} {
		key := fmt.Sprintf("%s%s/%s.json", prefix, date, date)
		if _, err := f.service.store.Put(ctx, key, strings.NewReader("event"), 5, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	p := sourceSweepCheckpoint{Round: 1}
	if err := f.service.initializeDiagnosticEventRound(ctx, prefix, &p, firstStart); err != nil {
		t.Fatal(err)
	}
	if p.EventRoundEndDate != firstCutoff.Format("20060102") || p.EventPartitionDate != olderPartition {
		t.Fatalf("first event round boundary = %q/%q", p.EventRoundEndDate, p.EventPartitionDate)
	}
	if err := f.service.store.Delete(ctx, fmt.Sprintf("%s%s/%s.json", prefix, olderPartition, olderPartition)); err != nil {
		t.Fatal(err)
	}
	p.Status.Complete = true
	f.service.completeSourceRound("diagnostic_retention_events", &p)
	secondStart := firstStart.Add(24 * time.Hour)
	if err := f.service.initializeDiagnosticEventRound(ctx, prefix, &p, secondStart); err != nil {
		t.Fatal(err)
	}
	if p.EventRoundEndDate != secondStart.Add(-diagnostics.Retention).Format("20060102") || p.EventPartitionDate != cutoffPartition {
		t.Fatalf("second event round boundary = %q/%q, want cutoff advanced to %q", p.EventRoundEndDate, p.EventPartitionDate, cutoffPartition)
	}
}
