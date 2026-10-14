package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"testing"
)

func newReferrerRepairTestService(t *testing.T, server *httptest.Server) (*Service, *tasks.Manager, *distribution.Client, *filesystem.Store, route.Route) {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		registry.Close()
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		registry.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	gate, err := newWriteGate(context.Background(), store)
	if err != nil {
		manager.Close()
		registry.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	service := &Service{
		cfg:    config.Config{Limits: config.Limits{ManifestBytes: 1 << 20, MaxDescriptors: 100, DescriptorDataBytes: 1 << 20}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store, registry: registry,
		tasks: manager, gate: gate, metrics: newMetrics(), solidificationSlots: make(chan struct{}, 4),
	}
	routed, err := taskRoute("ghcr.io", "team/image")
	if err != nil {
		manager.Close()
		registry.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	return service, manager, registry, store, routed
}
