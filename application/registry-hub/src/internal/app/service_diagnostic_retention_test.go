package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestDiagnosticRetentionUsesConfiguredShortDurations(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.EventRetention = 5 * time.Second
	f.service.cfg.Diagnostics.EventBudgetRetention = 6 * time.Second
	f.service.cfg.Diagnostics.CollectedReceiptRetention = 7 * time.Second
	f.service.cfg.Diagnostics.TransferObservationRetention = 8 * time.Second
	ctx := context.Background()
	tests := []struct {
		name      string
		key       string
		retention time.Duration
	}{
		{name: "legacy event", key: diagnostics.Prefix + "events/20261122/event.json", retention: 5 * time.Second},
		{name: "event batch", key: diagnostics.BatchPrefix + "20261122/batch.json", retention: 5 * time.Second},
		{name: "event budget", key: diagnostics.Prefix + "event-budgets/20261122.json", retention: 6 * time.Second},
		{name: "collection receipt", key: diagnostics.Prefix + "collected/source.json", retention: 7 * time.Second},
		{name: "transfer session", key: diagnostics.Prefix + "transfer-sessions/session.json", retention: 8 * time.Second},
		{name: "upload receipt", key: diagnostics.Prefix + "upload-receipts/receipt.json", retention: 8 * time.Second},
		{name: "transfer progress", key: diagnostics.Prefix + "transfer-progress/progress.json", retention: 8 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, err := f.service.store.Put(ctx, test.key, strings.NewReader("{}"), 2, storage.PutOptions{IfAbsent: true})
			if err != nil {
				t.Fatal(err)
			}
			entry.ModTime = time.Now().Add(-test.retention - time.Second).UnixNano()
			var progress sourceSweepCheckpoint
			if err := f.service.cleanupDiagnosticRetentionEntry(ctx, entry, &progress); err != nil {
				t.Fatalf("cleanup with configured retention: %v", err)
			}
			if _, err := f.service.store.Head(ctx, test.key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("expired object remains: %v", err)
			}
		})
	}
}

func TestRetryRequestRetentionUsesConfiguredShortDuration(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.RetryRequestRetention = time.Second
	ctx := context.Background()
	request := workqueue.RetryAcceptance{ID: "expired-retry-request", Kind: "notify", WorkID: "missing-work"}
	key := workqueue.Prefix + "retry-requests/" + request.ID + ".json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.RetryAcceptance) error {
		*value = request
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	entry.ModTime = time.Now().Add(-2 * time.Second).UnixNano()
	if err := f.service.cleanupRetryRequestEntry(ctx, entry); err != nil {
		t.Fatalf("cleanup expired retry request: %v", err)
	}
	if _, err := f.service.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired retry request remains: %v", err)
	}
}

func TestTerminalTransferExpiryUsesConfiguredRetention(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.TerminalHistoryRetention = time.Minute
	f.service.cfg.Diagnostics.TransferExpiryRetention = 45 * time.Second
	terminalAt := time.Now().UTC()
	view := transferView{
		ID: diagnostics.ID("client_upload", "configured-expiry"), Kind: "client_upload",
		TransferState: "complete", FinishedAt: terminalAt, ProgressAt: terminalAt,
		ObservedAt: terminalAt, EvidenceSource: "test",
	}
	writeTransferHistoryFixture(t, f.service, view)
	version := sourceEntryVersionFromStore(t, f.service.store, diagnostics.Prefix+"transfers/"+view.ID+".json")
	if err := f.service.projectTerminalTransferHistory(context.Background(), view, version); err != nil {
		t.Fatal(err)
	}
	expiry, _, err := inventory.Read[terminalTransferExpiry](context.Background(), f.service.store, terminalTransferExpiryKey(view.ID))
	if err != nil {
		t.Fatal(err)
	}
	if want := terminalAt.Add(45 * time.Second); !expiry.ExpiresAt.Equal(want) {
		t.Fatalf("transfer expiry = %s, want configured expiry %s", expiry.ExpiresAt, want)
	}
}

func TestDiagnosticEventReclaimLinksPointToActiveRetentionWorkers(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	for _, test := range []struct {
		key  string
		want string
	}{
		{key: diagnostics.Prefix + "events/20261122/event.json", want: "diagnostic_retention_events"},
		{key: diagnostics.BatchPrefix + "20261122/batch.json", want: "diagnostic_retention_event_batches"},
	} {
		entry, err := f.service.store.Put(ctx, test.key, strings.NewReader("{}"), 2, storage.PutOptions{IfAbsent: true})
		if err != nil {
			t.Fatal(err)
		}
		object := storageObject{Domain: "state", Category: "diagnostic_events"}
		f.service.inspectReclaimEvidence(ctx, entry, &object)
		if object.Cleaner != test.want || object.CleanerURL != "/api/v1/workers/"+test.want {
			t.Fatalf("reclaim link for %q = %s %q, want %s", test.key, object.Cleaner, object.CleanerURL, test.want)
		}
	}
}
