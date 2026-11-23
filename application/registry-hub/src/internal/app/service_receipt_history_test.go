package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"

	"registry-hub/internal/workqueue"
)

func TestTerminalWorkHistoryProjectionPagesByCompletionAndExpiresDetails(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	now := time.Now().UTC()
	ids := []string{workqueue.ID("notify", "newest"), workqueue.ID("notify", "middle"), workqueue.ID("notify", "oldest")}
	completed := []time.Time{now.Add(-time.Minute), now.Add(-time.Hour), now.Add(-2 * time.Hour)}
	for index, id := range ids {
		payload, _ := json.Marshal(map[string]string{"source": fmt.Sprintf("source-%d", index)})
		job := workqueue.Job{SchemaVersion: 1, ID: id, Kind: "notify", Payload: payload, State: "done", CreatedAt: now.Add(-3 * time.Hour), CompletedAt: completed[index]}
		entry := putTestReceipt(t, f.service, job)
		projected, err := f.service.projectTerminalWorkHistory(ctx, entry.Key, sourceEntryVersion(entry), entry, job)
		if err != nil || !projected {
			t.Fatalf("project terminal job %d: projected=%t err=%v", index, projected, err)
		}
	}
	seen := make([]string, 0, len(ids))
	path := "/api/v1/background?state=done&limit=2"
	for pageNumber := 0; path != ""; pageNumber++ {
		response := diagnosticGet(t, f.service, path)
		var page backgroundJobPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("history page %d: status=%d body=%s", pageNumber, response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/background?state=done&limit=2&token=" + page.NextToken
		}
	}
	if len(seen) != len(ids) || seen[0] != ids[0] || seen[1] != ids[1] || seen[2] != ids[2] {
		t.Fatalf("terminal history order = %v, want %v", seen, ids)
	}
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+ids[1]); response.Code != http.StatusOK {
		t.Fatalf("projected detail status=%d body=%s", response.Code, response.Body.String())
	}

	expiredID := workqueue.ID("notify", "expired")
	expiredPayload, _ := json.Marshal(map[string]string{"source": "expired"})
	expired := workqueue.Job{SchemaVersion: 1, ID: expiredID, Kind: "notify", Payload: expiredPayload, State: "done", CreatedAt: now.Add(-50 * time.Hour), CompletedAt: now.Add(-49 * time.Hour)}
	putTestReceipt(t, f.service, expired)
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+expiredID); response.Code != http.StatusGone {
		t.Fatalf("expired detail status=%d body=%s", response.Code, response.Body.String())
	}
	unknownID := workqueue.ID("notify", "unknown")
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+unknownID); response.Code != http.StatusNotFound {
		t.Fatalf("unknown detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTerminalTransferHistoryProjectionUsesFortyEightHourWindow(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	now := time.Now().UTC()
	current := transferView{ID: diagnostics.ID("client_upload", "history-current"), Kind: "client_upload", TransferState: "complete", FinishedAt: now.Add(-time.Hour), ProgressAt: now.Add(-time.Hour), ObservedAt: now, EvidenceSource: "test"}
	writeTransferHistoryFixture(t, f.service, current)
	if err := f.service.projectTerminalTransferHistory(context.Background(), current, sourceEntryVersionFromStore(t, f.service.store, diagnostics.Prefix+"transfers/"+current.ID+".json")); err != nil {
		t.Fatal(err)
	}
	old := transferView{ID: diagnostics.ID("client_upload", "history-expired"), Kind: "client_upload", TransferState: "cancelled", FinishedAt: now.Add(-49 * time.Hour), ProgressAt: now.Add(-49 * time.Hour), ObservedAt: now, EvidenceSource: "test"}
	writeTransferHistoryFixture(t, f.service, old)
	if err := f.service.projectTerminalTransferHistory(context.Background(), old, sourceEntryVersionFromStore(t, f.service.store, diagnostics.Prefix+"transfers/"+old.ID+".json")); err != nil {
		t.Fatal(err)
	}
	seen := false
	path := "/api/v1/transfers?limit=1"
	for pages := 0; path != ""; pages++ {
		response := diagnosticGet(t, f.service, path)
		var page transferHistoryPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("transfer history page: status=%d body=%s", response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			if item.ID == current.ID {
				seen = true
			}
			if item.ID == old.ID {
				t.Fatalf("expired transfer appeared in list: %+v", item)
			}
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/transfers?limit=1&token=" + page.NextToken
		}
		if pages > 3 {
			t.Fatal("transfer history pagination did not terminate")
		}
	}
	if !seen {
		t.Fatal("recent terminal transfer missing from history")
	}
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+current.ID); response.Code != http.StatusOK {
		t.Fatalf("recent transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
	oldEntry, err := f.service.store.Head(context.Background(), diagnostics.Prefix+"transfers/"+old.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.store.DeleteIfMatch(context.Background(), oldEntry.Key, oldEntry.Version); err != nil {
		t.Fatal(err)
	}
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+old.ID); response.Code != http.StatusGone {
		t.Fatalf("expired transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
	unknownID := diagnostics.ID("client_upload", "unknown-transfer")
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+unknownID); response.Code != http.StatusNotFound {
		t.Fatalf("unknown transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTerminalTransferProjectionRejectsChangedSourceVersion(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	view := transferView{ID: diagnostics.ID("client_upload", "version-check"), Kind: "client_upload", TransferState: "complete", FinishedAt: time.Now().UTC(), ProgressAt: time.Now().UTC()}
	writeTransferHistoryFixture(t, f.service, view)
	if err := f.service.projectTerminalTransferHistory(context.Background(), view, "stale-source-version"); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("project stale transfer source error = %v, want condition failure", err)
	}
	if _, _, err := inventory.Read[terminalTransferHistoryRecord](context.Background(), f.service.store, terminalTransferHistoryKey(view.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale transfer projection exists: %v", err)
	}
}

func TestIndexedTerminalWorkHistoryAvoidsReceiptEnumeration(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	now := time.Now().UTC()
	tracked := &historyListCountingStore{Store: f.service.store}
	f.service.store = tracked
	for index := 0; index < 3; index++ {
		id := workqueue.ID("notify", fmt.Sprintf("indexed-%d", index))
		payload, _ := json.Marshal(map[string]string{"source": fmt.Sprintf("indexed-source-%d", index)})
		job := workqueue.Job{SchemaVersion: 1, ID: id, Kind: "notify", Payload: payload, State: "done", CompletedAt: now.Add(-time.Duration(index+1) * time.Minute)}
		entry := putTestReceipt(t, f.service, job)
		if projected, err := f.service.projectTerminalWorkHistory(ctx, entry.Key, sourceEntryVersion(entry), entry, job); err != nil || !projected {
			t.Fatalf("project indexed work history: projected=%t err=%v", projected, err)
		}
	}
	cursor := diagnosticCursor{HistorySource: terminalWorkHistoryIndexSource, HistoryBefore: now.Add(-f.service.diagnosticsConfig().TerminalHistoryRetention)}
	rows, scanned, more, fallback, err := f.service.listTerminalWorkHistory(ctx, nilValues(), &cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || scanned != 2 || !more || fallback {
		t.Fatalf("indexed page rows=%d scanned=%d more=%t fallback=%t", len(rows), scanned, more, fallback)
	}
	budgetInitializationScans := 0
	for _, prefix := range tracked.listedPrefixes {
		if strings.HasPrefix(prefix, workqueue.Prefix+"receipts/") {
			t.Fatalf("indexed listing enumerated legacy receipt prefix %q", prefix)
		}
		if prefix == terminalWorkHistoryPrefix {
			budgetInitializationScans++
		}
	}
	if budgetInitializationScans != 1 {
		t.Fatalf("projection budget was initialized %d times, want one metadata scan", budgetInitializationScans)
	}
}

type historyListCountingStore struct {
	storage.Store
	listedPrefixes []string
}

func (s *historyListCountingStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.listedPrefixes = append(s.listedPrefixes, prefix)
	return s.Store.List(ctx, prefix, token, limit)
}

func nilValues() url.Values { return url.Values{} }

func putTestReceipt(t *testing.T, service *Service, job workqueue.Job) storage.Entry {
	t.Helper()
	ctx := context.Background()
	key := workqueue.ReceiptKey(job.Kind, job.ID)
	if err := inventory.Update(ctx, service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
		t.Fatal(err)
	}
	entry, err := service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	putReceiptArchiveProjection(t, service, entry)
	return entry
}

func putReceiptArchiveProjection(t *testing.T, service *Service, entry storage.Entry) {
	t.Helper()
	projection := diagnosticReceiptArchive{
		SourceKey: entry.Key, SourceVersion: sourceEntryVersion(entry), ProjectionVersion: diagnosticReceiptProjectionVersion,
		HistoryProjected: true, OutboxComplete: true, OutboxDispositionVersion: 1, Complete: true,
	}
	key := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(entry.Key) + ".json"
	if err := inventory.Update(context.Background(), service.store, key, func(value *diagnosticReceiptArchive) error { *value = projection; return nil }); err != nil {
		t.Fatal(err)
	}
}

func writeTransferHistoryFixture(t *testing.T, service *Service, view transferView) {
	t.Helper()
	key := diagnostics.Prefix + "transfers/" + view.ID + ".json"
	if err := inventory.Update(context.Background(), service.store, key, func(value *transferView) error { *value = view; return nil }); err != nil {
		t.Fatal(err)
	}
}
