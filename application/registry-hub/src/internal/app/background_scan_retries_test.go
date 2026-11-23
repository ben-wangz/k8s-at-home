package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestSourceSweepPersistsRetryBeforeAdvancingCursor(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	entry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt"))
	if err != nil {
		t.Fatal(err)
	}
	p := sourceSweepCheckpoint{Round: 1, Backoff: map[string]entityBackoff{}}
	if f.service.sourceSweepEntity(ctx, "history_receipts", "test/receipts/", &p, func(context.Context, storage.Entry, *sourceSweepCheckpoint) error {
		return errors.New("temporary_failure")
	}) {
		t.Fatal("one-entry source sweep unexpectedly requested another item")
	}
	if p.Cursor.Entities != 1 || p.Status.Counts["failed"] != 1 {
		t.Fatalf("source cursor/status = %#v, want one durably failed item", p)
	}
	retryEntries, _, err := f.service.store.List(ctx, sourceRetryPrefix+"history_receipts/", "", 10)
	if err != nil || len(retryEntries) != 1 {
		t.Fatalf("source retry records=%#v err=%v, want one", retryEntries, err)
	}
	retry, _, err := inventory.Read[sourceSweepRetry](ctx, f.service.store, retryEntries[0].Key)
	if err != nil || retry.State != "pending" || retry.SourceKey != entry.Key || retry.SourceVersion == "" {
		t.Fatalf("source retry = %#v err=%v", retry, err)
	}
	if due, _, err := f.service.store.List(ctx, sourceRetryDuePrefix+"history_receipts/", "", 10); err != nil || len(due) != 1 {
		t.Fatalf("retry due index = %#v err=%v, want one discoverable retry", due, err)
	}
}

func TestDueSourceRetryDispatchesAndFencesSourceVersion(t *testing.T) {
	for _, mode := range []string{"complete", "reschedule", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			entry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt"))
			if err != nil {
				t.Fatal(err)
			}
			kind := "history_receipts"
			next := time.Now().UTC().Add(-time.Second)
			if err := f.service.enqueueSourceSweepRetry(ctx, kind, entry, errors.New("temporary_failure"), 1, next); err != nil {
				t.Fatal(err)
			}
			retryID := sourceRetryID(kind, "default", entry.Key, sourceEntryIdentity(entry))
			oldDueKey := sourceRetryDueKey(kind, next, retryID, 1)
			if mode == "superseded" {
				if _, err := f.service.store.Put(ctx, entry.Key, bytes.NewReader([]byte("replacement")), int64(len("replacement")), storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			manager := lease.NewManager(f.service.store)
			holder, err := manager.Acquire(ctx, "background/source/"+kind, "retry-test", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Release(ctx, holder)
			calls := 0
			processed, err := f.service.dispatchDueSourceSweepRetry(ctx, holder, kind, &sourceSweepCheckpoint{}, func(context.Context, storage.Entry, *sourceSweepCheckpoint) error {
				calls++
				if mode == "reschedule" {
					return errors.New("still_unavailable")
				}
				return nil
			})
			if err != nil || !processed {
				t.Fatalf("dispatch retry processed=%t err=%v", processed, err)
			}
			if mode == "superseded" {
				if calls != 0 {
					t.Fatalf("superseded source was processed %d times", calls)
				}
			} else if calls != 1 {
				t.Fatalf("retry step calls=%d, want one", calls)
			}
			records, _, err := f.service.store.List(ctx, sourceRetryPrefix+kind+"/", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "reschedule" {
				if len(records) != 1 {
					t.Fatalf("rescheduled retry records=%d, want one", len(records))
				}
				retry, _, err := inventory.Read[sourceSweepRetry](ctx, f.service.store, records[0].Key)
				if err != nil || retry.Revision != 2 || retry.Attempts != 2 || !retry.NextAttemptAt.After(time.Now()) {
					t.Fatalf("rescheduled retry = %#v err=%v", retry, err)
				}
				due, _, err := f.service.store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 10)
				if err != nil || len(due) != 1 || due[0].Key == oldDueKey {
					t.Fatalf("rescheduled due index=%#v err=%v", due, err)
				}
			} else if len(records) != 0 {
				t.Fatalf("completed retry records=%d, want none", len(records))
			}
		})
	}
}

func TestSourceSweepReadFailureDoesNotAdvanceCursorAndRecovers(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	if _, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt")); err != nil {
		t.Fatal(err)
	}
	faults := &sourceSweepFaultStore{Store: f.service.store, failListPrefix: "test/receipts/", failListErr: storage.ErrUnavailable}
	f.service.store = faults
	p := sourceSweepCheckpoint{Round: 1, Backoff: map[string]entityBackoff{}}
	calls := 0
	step := func(context.Context, storage.Entry, *sourceSweepCheckpoint) error { calls++; return nil }
	if f.service.sourceSweepEntity(ctx, "history_receipts", "test/receipts/", &p, step) {
		t.Fatal("failed source read unexpectedly requested another item")
	}
	if calls != 0 || p.Cursor.Entities != 0 || p.Cursor.Offset != 0 || p.ScanRetries != 1 {
		t.Fatalf("after failed source read: calls=%d cursor=%#v retries=%d", calls, p.Cursor, p.ScanRetries)
	}
	f.service.sourceSweepEntity(ctx, "history_receipts", "test/receipts/", &p, step)
	if calls != 1 || p.Cursor.Entities != 1 {
		t.Fatalf("source read recovery: calls=%d cursor=%#v", calls, p.Cursor)
	}
}

func TestSourceRetryUnknownCommitWritesRecover(t *testing.T) {
	for _, test := range []struct {
		name           string
		keyPrefix      string
		afterCommit    bool
		wantEnqueueErr bool
	}{
		{name: "record before commit", keyPrefix: sourceRetryPrefix + "history_receipts/"},
		{name: "record after commit", keyPrefix: sourceRetryPrefix + "history_receipts/", afterCommit: true},
		{name: "due index before commit", keyPrefix: sourceRetryDuePrefix + "history_receipts/", wantEnqueueErr: true},
		{name: "due index after commit", keyPrefix: sourceRetryDuePrefix + "history_receipts/", afterCommit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			entry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt"))
			if err != nil {
				t.Fatal(err)
			}
			faults := &sourceSweepFaultStore{Store: f.service.store, unknownPutPrefix: test.keyPrefix, unknownAfterCommit: test.afterCommit}
			f.service.store = faults
			next := time.Now().UTC().Add(time.Minute)
			err = f.service.enqueueSourceSweepRetry(ctx, "history_receipts", entry, errors.New("temporary_failure"), 1, next)
			if test.wantEnqueueErr != (err != nil) || !faults.unknownPutFired {
				t.Fatalf("enqueue unknown commit fired=%t err=%v, want error=%t", faults.unknownPutFired, err, test.wantEnqueueErr)
			}
			p := sourceSweepCheckpoint{Round: 1}
			if err != nil {
				if repairErr := f.service.repairSourceRetryIndexes(ctx, "history_receipts", &p, 10); repairErr != nil {
					t.Fatalf("repair due index after partial commit: %v", repairErr)
				}
			}
			due, _, err := f.service.store.List(ctx, sourceRetryDuePrefix+"history_receipts/", "", 10)
			if err != nil || len(due) != 1 {
				t.Fatalf("due index after recovery = %#v err=%v, want one", due, err)
			}
		})
	}
}

func TestDiagnosticEventRetryRetainsDatePartition(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	key := "diagnostics/v1/events/20261120/event.json"
	entry, err := putSourceSweepFixture(ctx, f.service.store, key, []byte("event"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.enqueueSourceSweepRetry(ctx, "diagnostic_retention_events", entry, errors.New("temporary_failure"), 1, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	id := sourceRetryID("diagnostic_retention_events", "20261120", key, sourceEntryIdentity(entry))
	retry, _, err := inventory.Read[sourceSweepRetry](ctx, f.service.store, sourceRetryRecordKey("diagnostic_retention_events", id))
	if err != nil || retry.Partition != "20261120" || retry.ID != id {
		t.Fatalf("diagnostic event retry = %#v err=%v, want partition-scoped ID", retry, err)
	}
}

func putSourceSweepFixture(ctx context.Context, store storage.Store, key string, value []byte) (storage.Entry, error) {
	return store.Put(ctx, key, bytes.NewReader(value), int64(len(value)), storage.PutOptions{})
}

type sourceSweepFaultStore struct {
	storage.Store
	failListPrefix     string
	failListErr        error
	unknownPutPrefix   string
	unknownAfterCommit bool
	unknownPutFired    bool
}

func (s *sourceSweepFaultStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if s.failListErr != nil && strings.HasPrefix(prefix, s.failListPrefix) {
		err := s.failListErr
		s.failListErr = nil
		return nil, "", err
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func (s *sourceSweepFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if !s.unknownPutFired && s.unknownPutPrefix != "" && strings.HasPrefix(key, s.unknownPutPrefix) {
		s.unknownPutFired = true
		if !s.unknownAfterCommit {
			return storage.Entry{}, storage.ErrUnknownCommit
		}
		data, err := io.ReadAll(body)
		if err != nil {
			return storage.Entry{}, err
		}
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(data), size, options); err != nil {
			return storage.Entry{}, err
		}
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return s.Store.Put(ctx, key, body, size, options)
}
