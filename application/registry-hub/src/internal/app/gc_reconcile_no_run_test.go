package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

type reconcileFaultStore struct {
	storage.Store
	failGateRead bool
	rejectOpen   bool
	unknownOpen  bool
}

func (s *reconcileFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == "system/gc/state.json" && s.failGateRead {
		s.failGateRead = false
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *reconcileFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if key == "system/gc/state.json" {
		var state gateRecord
		if err := json.Unmarshal(data, &state); err != nil {
			return storage.Entry{}, err
		}
		if state.State == "open" && s.rejectOpen {
			s.rejectOpen = false
			return storage.Entry{}, storage.ErrConditionFailed
		}
		if state.State == "open" && s.unknownOpen {
			s.unknownOpen = false
			entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
			if err != nil {
				return entry, err
			}
			return storage.Entry{}, storage.ErrUnknownCommit
		}
	}
	return s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
}

func TestReconcileNoRunOpensOnlyAfterAllSafetyChecks(t *testing.T) {
	t.Run("GC disabled with explicit no-executor confirmation", func(t *testing.T) {
		ctx := context.Background()
		base, err := filesystem.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer base.Close()
		store := &reconcileFaultStore{Store: base}
		gate := newReconcileFailedUnknownGate(t, ctx, store)
		controller := &gcController{service: &Service{cfg: config.Config{GCMode: "disabled"}, store: store, gate: gate}}
		if _, err := controller.ReconcileNoRun(ctx, true); err != nil {
			t.Fatalf("ReconcileNoRun() error = %v", err)
		}
		if state := gate.State(); state.State != "open" || state.ExecutorID != "" || state.RecoveryBarrier {
			t.Fatalf("gate after reconciliation = %#v, want open with no executor or barrier", state)
		}
	})

	t.Run("missing confirmation and active executor are refused", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			confirmed  bool
			active     bool
			executorID string
			wantReason string
		}{
			{name: "confirmation", wantReason: "explicit confirmation"},
			{name: "active executor", confirmed: true, active: true, wantReason: "executor may still be active"},
			{name: "uncertain executor identity", confirmed: true, executorID: "external:unknown", wantReason: "executor whose state must be verified"},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx := context.Background()
				base, err := filesystem.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer base.Close()
				store := &reconcileFaultStore{Store: base}
				gate := newReconcileFailedUnknownGate(t, ctx, store)
				if test.executorID != "" {
					if err := gate.SetGCState(ctx, "failed_unknown", test.executorID, "storage"); err != nil {
						t.Fatal(err)
					}
				}
				controller := &gcController{service: &Service{cfg: config.Config{GCMode: "disabled"}, store: store, gate: gate}}
				if test.active {
					controller.cancel = func() {}
				}
				_, err = controller.ReconcileNoRun(ctx, test.confirmed)
				var conflict *gcReconciliationConflict
				if !errors.As(err, &conflict) || !strings.Contains(conflict.reason, test.wantReason) {
					t.Fatalf("ReconcileNoRun() error = %v, want conflict containing %q", err, test.wantReason)
				}
				if state := gate.State(); state.State != "failed_unknown" {
					t.Fatalf("gate after refused reconciliation = %q, want failed_unknown", state.State)
				}
			})
		}
	})

	t.Run("active protection and inconsistent totals are refused", func(t *testing.T) {
		for _, test := range []string{"active protection", "inconsistent totals"} {
			t.Run(test, func(t *testing.T) {
				ctx := context.Background()
				base, err := filesystem.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer base.Close()
				store := &reconcileFaultStore{Store: base}
				gate, err := newWriteGate(ctx, store)
				if err != nil {
					t.Fatal(err)
				}
				if test == "active protection" {
					if err := gate.AdmitProtection(ctx, mustGateTestID(t), "scope", "", 32, 4, 1024); err != nil {
						t.Fatal(err)
					}
				} else {
					gate.state.ReservedDistributionBytes = 32
				}
				if err := gate.SetGCState(ctx, "failed_unknown", "", "storage"); err != nil {
					t.Fatal(err)
				}
				controller := &gcController{service: &Service{cfg: config.Config{GCMode: "disabled"}, store: store, gate: gate}}
				_, err = controller.ReconcileNoRun(ctx, true)
				var conflict *gcReconciliationConflict
				if !errors.As(err, &conflict) {
					t.Fatalf("ReconcileNoRun() error = %v, want safety conflict", err)
				}
				if state := gate.State(); state.State != "failed_unknown" {
					t.Fatalf("gate after refused reconciliation = %q, want failed_unknown", state.State)
				}
			})
		}
	})

	t.Run("durable run record and enabled GC are refused", func(t *testing.T) {
		ctx := context.Background()
		base, err := filesystem.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer base.Close()
		store := &reconcileFaultStore{Store: base}
		gate := newReconcileFailedUnknownGate(t, ctx, store)
		data := []byte(`{"schemaVersion":1,"id":"0123456789abcdef0123456789abcdef","state":"failed_unknown"}`)
		if _, err := store.Put(ctx, "system/gc/last-run.json", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
		controller := &gcController{service: &Service{cfg: config.Config{GCMode: "disabled"}, store: store, gate: gate}}
		if _, err := controller.ReconcileNoRun(ctx, true); err == nil {
			t.Fatal("ReconcileNoRun() accepted an existing durable run record")
		}
		controller.service.cfg.GCMode = "local"
		if _, err := controller.ReconcileNoRun(ctx, true); err == nil {
			t.Fatal("ReconcileNoRun() accepted enabled GC mode")
		}
		if state := gate.State(); state.State != "failed_unknown" {
			t.Fatalf("gate after refused reconciliation = %q, want failed_unknown", state.State)
		}
	})
}

func newReconcileFailedUnknownGate(t *testing.T, ctx context.Context, store storage.Store) *writeGate {
	t.Helper()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.SetGCState(ctx, "failed_unknown", "", "storage"); err != nil {
		t.Fatal(err)
	}
	return gate
}
