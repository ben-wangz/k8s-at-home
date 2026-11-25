package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
)

func TestDiagnosticRetentionUsesConfiguredShortDurations(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.EventRetention = 5 * time.Second
	f.service.cfg.Diagnostics.EventBudgetRetention = 6 * time.Second
	f.service.cfg.Diagnostics.CollectedReceiptRetention = 7 * time.Second
	f.service.cfg.Diagnostics.TransferObservationRetention = 8 * time.Second
	ctx := context.Background()
	expiredEventDate := time.Now().UTC().Add(-48 * time.Hour).Format("20060102")
	tests := []struct {
		name      string
		key       string
		retention time.Duration
		body      string
		protected bool
	}{
		{name: "legacy event", key: diagnostics.Prefix + "events/" + expiredEventDate + "/event.json", retention: 5 * time.Second},
		{name: "event batch", key: diagnostics.BatchPrefix + expiredEventDate + "/batch.json", retention: 5 * time.Second},
		{name: "event budget", key: diagnostics.Prefix + "event-budgets/20261122.json", retention: 6 * time.Second},
		{name: "collection receipt", key: diagnostics.Prefix + "collected/source.json", retention: 7 * time.Second},
		{name: "pending collection receipt", key: diagnostics.Prefix + "collected/unsettled.json", retention: 7 * time.Second, body: `{"lastEventID":"pending"}`, protected: true},
		{name: "transfer session", key: diagnostics.Prefix + "transfer-sessions/session.json", retention: 8 * time.Second, body: `{"kind":"upstream_download","cacheCommitState":"committed","finishedAt":"2020-01-01T00:00:00Z"}`},
		{name: "active transfer session", key: diagnostics.Prefix + "transfer-sessions/active.json", retention: 8 * time.Second, body: `{"kind":"upstream_download","transferState":"in_progress"}`, protected: true},
		{name: "upload receipt", key: diagnostics.Prefix + "upload-receipts/receipt.json", retention: 8 * time.Second, body: `{"kind":"client_upload","transferState":"complete","finishedAt":"2020-01-01T00:00:00Z"}`},
		{name: "transfer progress", key: diagnostics.Prefix + "transfer-progress/progress.json", retention: 8 * time.Second, body: `{"finishedAt":"2020-01-01T00:00:00Z"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			if body == "" {
				body = "{}"
			}
			entry, err := f.service.store.Put(ctx, test.key, strings.NewReader(body), int64(len(body)), storage.PutOptions{IfAbsent: true})
			if err != nil {
				t.Fatal(err)
			}
			entry.ModTime = time.Now().Add(-test.retention - time.Second).UnixNano()
			var progress sourceSweepCheckpoint
			if err := f.service.cleanupDiagnosticRetentionEntry(ctx, entry, &progress); err != nil {
				t.Fatalf("cleanup with configured retention: %v", err)
			}
			_, err = f.service.store.Head(ctx, test.key)
			if test.protected && err != nil {
				t.Fatalf("protected object was removed: %v", err)
			}
			if !test.protected && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("expired object remains: %v", err)
			}
			if strings.HasPrefix(test.key, diagnostics.Prefix+"events/") || strings.HasPrefix(test.key, diagnostics.BatchPrefix) {
				date, _ := historyCleanupEventDate(test.key)
				day, parseErr := time.Parse("20060102", date)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				sealed, sealErr := diagnostics.EventTimeSealed(ctx, f.service.store, day.Add(12*time.Hour))
				if sealErr != nil || !sealed {
					t.Fatalf("automatic event cleanup did not seal its complete UTC day: sealed=%t error=%v", sealed, sealErr)
				}
			}
		})
	}
}

func TestHistoryProjectionBudgetReleaseRecoversAfterProjectionWasDeleted(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &historyBudgetWriteFaultStore{Store: base}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	key := terminalWorkHistoryPrefix + "deleted-before-budget-release.json"
	if _, err := f.service.reserveHistoryProjection(ctx, terminalWorkHistoryPrefix, key, 12, 10, 1024); err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Put(ctx, key, strings.NewReader(strings.Repeat("x", 12)), 12, storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.store.DeleteIfMatch(ctx, key, entry.Version); err != nil {
		t.Fatal(err)
	}
	store.failBudgetWrite = true
	if err := f.service.releaseHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, key, entry.Size); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("first budget release error = %v, want unavailable", err)
	}
	var progress sourceSweepCheckpoint
	if err := f.service.cleanupDiagnosticRetentionEntry(ctx, entry, &progress); err != nil {
		t.Fatalf("retry missing projection cleanup: %v", err)
	}
	budgetKey := diagnostics.Prefix + "history-budgets/" + inventory.ID(terminalWorkHistoryPrefix) + ".json"
	budget, _, err := inventory.Read[historyProjectionBudget](ctx, f.service.store, budgetKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := budget.Sizes[key]; exists || budget.TotalBytes != 0 {
		t.Fatalf("budget remained charged after retry: %+v", budget)
	}
}

type historyBudgetWriteFaultStore struct {
	storage.Store
	failBudgetWrite bool
}

func (store *historyBudgetWriteFaultStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if store.failBudgetWrite && strings.HasPrefix(key, diagnostics.Prefix+"history-budgets/") {
		store.failBudgetWrite = false
		return storage.Entry{}, storage.ErrUnavailable
	}
	return store.Store.Put(ctx, key, reader, size, options)
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
