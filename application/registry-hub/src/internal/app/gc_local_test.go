package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/storage/filesystem"
)

func TestLocalGCExecutesConfiguredCommandAndReopensGate(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, "distribution")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDirectory, "config.yml")
	if err := os.WriteFile(configPath, []byte("version: 0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(root, "gc-command.txt")
	t.Setenv("REGISTRY_HUB_GC_TEST_RECORD", recordPath)
	executablePath := filepath.Join(root, "registry")
	script := "#!/bin/sh\nprintf 'cwd=%s\\n' \"$PWD\" > \"$REGISTRY_HUB_GC_TEST_RECORD\"\nprintf 'arg=%s\\n' \"$@\" >> \"$REGISTRY_HUB_GC_TEST_RECORD\"\n"
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := filesystem.New(filepath.Join(root, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer registryServer.Close()
	registryURL, err := url.Parse(registryServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: registryURL})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		cfg:      config.Config{GCMode: "local", GCExecutable: executablePath, GCConfig: configPath},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		registry: registry,
		gate:     gate,
		uploads:  &uploadSessions{store: store, registry: registry},
		metrics:  newMetrics(),
		ctx:      ctx,
	}
	controller, err := newGCController(service)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	started, err := controller.Trigger(false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		controller.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("local GC did not finish: %v", ctx.Err())
	}

	finished := controller.Snapshot()
	if finished.ID != started.ID || finished.Mode != "local" || finished.State != "succeeded" || finished.ExecutorID != "local:"+started.ID {
		t.Fatalf("local GC run = (%q, %q, %q, %q)", finished.ID, finished.Mode, finished.State, finished.ExecutorID)
	}
	if gate.State().State != "open" {
		t.Fatalf("write gate state = %q, want open", gate.State().State)
	}
	contents, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cwd=" + configDirectory, "arg=garbage-collect", "arg=" + configPath}
	if got := strings.Split(strings.TrimSpace(string(contents)), "\n"); !equalStrings(got, want) {
		t.Fatalf("local GC executable invocation = %q, want %q", got, want)
	}
	object, err := store.Get(ctx, "system/gc/last-run.json")
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var persisted gcRun
	if err := json.NewDecoder(object.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ID != started.ID || persisted.State != "succeeded" || persisted.Mode != "local" {
		t.Fatalf("persisted local GC run = (%q, %q, %q)", persisted.ID, persisted.State, persisted.Mode)
	}
}
