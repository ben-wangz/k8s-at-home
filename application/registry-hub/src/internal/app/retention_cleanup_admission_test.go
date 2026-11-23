package app

import (
	"context"
	"testing"
	"time"
)

func TestRetentionCleanupAdmissionSerializesCleanupKinds(t *testing.T) {
	service := &Service{backgroundScheduler: &backgroundScheduler{capacity: 2}}
	firstRelease, admitted := service.backgroundKindBudget(context.Background(), "diagnostic_retention_events")
	if !admitted {
		t.Fatal("first retention cleanup was not admitted")
	}
	ordinaryRelease, admitted := service.backgroundKindBudget(context.Background(), "source_tasks")
	if !admitted {
		firstRelease()
		t.Fatal("ordinary background work could not share capacity with cleanup")
	}

	blocked, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, admitted := service.backgroundKindBudget(blocked, "receipt_cleanup"); admitted {
		ordinaryRelease()
		firstRelease()
		t.Fatal("second retention cleanup was admitted concurrently")
	}

	ordinaryRelease()
	firstRelease()
	manualRelease, admitted := service.backgroundKindBudget(context.Background(), historyCleanupWorkKind)
	if !admitted {
		t.Fatal("manual history cleanup was not admitted after the previous cleanup released its slot")
	}
	manualRelease()
}

func TestRetentionCleanupAdmissionReleasesSlotWhenBackgroundCapacityYields(t *testing.T) {
	service := &Service{backgroundScheduler: &backgroundScheduler{capacity: 1}}
	ordinaryRelease, admitted := service.backgroundKindBudget(context.Background(), "source_tasks")
	if !admitted {
		t.Fatal("ordinary background work was not admitted")
	}

	blocked, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, admitted = service.backgroundKindBudget(blocked, "receipt_cleanup")
	cancel()
	ordinaryRelease()
	if admitted {
		t.Fatal("cleanup was admitted after its background admission deadline")
	}

	release, admitted := service.backgroundKindBudget(context.Background(), "diagnostic_retention_events")
	if !admitted {
		t.Fatal("cleanup slot was not released after background capacity yielded")
	}
	release()
}

func TestWorkerViewsOmitRemovedDiagnosticRetentionAlias(t *testing.T) {
	service := &Service{}
	views := service.workerViews(backgroundStatus{Maintenance: map[string]backgroundScanStatus{}})
	seen := map[string]bool{}
	for _, view := range views {
		seen[view.ID] = true
	}
	if seen["diagnostic_retention"] {
		t.Fatal("removed aggregate diagnostic_retention worker is still advertised")
	}
	for _, kind := range []string{"diagnostic_retention_events", "diagnostic_retention_event_batches"} {
		if !seen[kind] {
			t.Fatalf("active retention worker %q is missing", kind)
		}
	}
}
