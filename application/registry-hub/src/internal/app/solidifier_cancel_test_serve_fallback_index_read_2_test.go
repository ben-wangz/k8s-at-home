package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func (fixture *fallbackPublicationFixture) serveFallbackIndexRead(writer http.ResponseWriter, request *http.Request) {
	fixture.objectMu.Lock()
	fixture.fallbackGets++
	data := append([]byte(nil), fixture.fallbackIndex...)
	fixture.objectMu.Unlock()
	if len(data) == 0 {
		fixture.startOnce.Do(func() { close(fixture.fallbackReadStarted) })
		select {
		case <-fixture.releaseFallbackRead:
		case <-request.Context().Done():
			return
		}
		writeManifestMiss(writer)
		return
	}
	writer.Header().Set("Content-Type", referrerIndexMediaType)
	writer.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(data))
	_, _ = writer.Write(data)
}

func (fixture *fallbackPublicationFixture) serveFallbackIndexWrite(writer http.ResponseWriter, request *http.Request, concurrentIndex []byte) {
	data, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "invalid body", http.StatusBadRequest)
		return
	}
	indexDigest := artifact.CalculateSHA256(data)
	fixture.checkPublicationLeases(request, indexDigest)
	fixture.objectMu.Lock()
	fixture.fallbackPuts++
	putNumber := fixture.fallbackPuts
	if putNumber == 1 {
		fixture.fallbackIndex = append([]byte(nil), concurrentIndex...)
	} else {
		fixture.fallbackIndex = append([]byte(nil), data...)
	}
	fixture.objectMu.Unlock()
	if putNumber == 1 {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	writer.Header().Set("Docker-Content-Digest", indexDigest)
	writer.WriteHeader(http.StatusCreated)
}

func (fixture *fallbackPublicationFixture) checkPublicationLeases(request *http.Request, indexDigest string) {
	for _, key := range []string{
		"referrers/" + objectLeaseKey("ghcr.io", "team/image", fixture.subjectDigest),
		tagLeaseKey("ghcr.io", "team/image", referrerFallbackTag(fixture.subjectDigest)),
		"objects/" + objectLeaseKey("ghcr.io", "team/image", indexDigest),
	} {
		probe, err := fixture.manager.Leases().Acquire(request.Context(), key, "acceptance-probe", time.Minute)
		if err == nil {
			_ = fixture.manager.Leases().Release(request.Context(), probe)
			fixture.recordLeaseProbeFailure(key + " was not held during index write")
		} else if !errors.Is(err, lease.ErrHeld) {
			fixture.recordLeaseProbeFailure(key + ": " + err.Error())
		}
	}
}

func (fixture *fallbackPublicationFixture) recordLeaseProbeFailure(message string) {
	fixture.objectMu.Lock()
	fixture.leaseProbeFailures = append(fixture.leaseProbeFailures, message)
	fixture.objectMu.Unlock()
}

func (fixture *fallbackPublicationFixture) beginPublication(t *testing.T) {
	t.Helper()
	task := fixture.task
	fixture.result = make(chan error, 1)
	go func() {
		fixture.result <- fixture.processor.syncReferrerManifest(fixture.ctx, &task, fixture.taskLease, fixture.manifestData, ociManifestMediaType, fixture.manifestDigest, "", false)
	}()
	select {
	case <-fixture.fallbackReadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback index publication did not reach its read-modify-write phase")
	}
}

func (fixture *fallbackPublicationFixture) assertCancellationKeepsProtection(t *testing.T) {
	t.Helper()
	persisted, err := fixture.manager.Get(fixture.ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.RootPublishing || persisted.RootPublished {
		t.Fatalf("fallback publication fence state = %#v", persisted)
	}
	cancelRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+fixture.task.ID+"/cancel", nil)
	cancelRequest.SetBasicAuth("operator", "secret")
	cancelResponse := httptest.NewRecorder()
	fixture.service.handleAdmin(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusAccepted {
		t.Fatalf("cancel during fallback index publication returned %d: %s", cancelResponse.Code, cancelResponse.Body.String())
	}
	var cancelView adminTaskView
	if err := json.Unmarshal(cancelResponse.Body.Bytes(), &cancelView); err != nil {
		t.Fatal(err)
	}
	if cancelView.ID != fixture.task.ID || cancelView.Status != tasks.Running || !cancelView.CancelRequested || !cancelView.GCProtectionActive {
		t.Fatalf("cancel response did not retain the running protection state: %#v", cancelView)
	}
	fixture.assertGCBlocked(t)
}

func (fixture *fallbackPublicationFixture) assertGCBlocked(t *testing.T) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/gc", nil)
	request.SetBasicAuth("operator", "secret")
	response := httptest.NewRecorder()
	fixture.service.handleAdmin(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"GC_BLOCKED"`) {
		t.Fatalf("GC after accepted non-quiescent cancellation returned %d: %s", response.Code, response.Body.String())
	}
	if fixture.service.gate.State().State != "open" || fixture.service.gate.State().ProtectionSessionCount != 1 {
		t.Fatalf("accepted cancellation changed the GC gate before quiescence: %#v", fixture.service.gate.State())
	}
}

func (fixture *fallbackPublicationFixture) completePublication(t *testing.T) {
	t.Helper()
	fixture.releaseOnce.Do(func() { close(fixture.releaseFallbackRead) })
	select {
	case err := <-fixture.result:
		if err != nil {
			t.Fatalf("syncReferrerManifest() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fallback index publication did not finish")
	}
	persisted, err := fixture.manager.Get(fixture.ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RootPublishing || persisted.RootPublished {
		t.Fatalf("completed non-root publication retained its fence: %#v", persisted)
	}
	persisted.Status = tasks.Cancelled
	if err := fixture.manager.SaveProgress(fixture.ctx, &persisted, fixture.taskLease); err != nil {
		t.Fatalf("save task quiescence after publication = %v", err)
	}
	if err := fixture.processor.TaskTerminal(fixture.ctx, &persisted); err != nil {
		t.Fatalf("release protection after task quiescence = %v", err)
	}
	if !persisted.TerminalCleanupComplete || persisted.GCProtected || fixture.service.gate.State().ProtectionSessionCount != 0 {
		t.Fatalf("quiescent task retained GC protection: task=%#v gate=%#v", persisted, fixture.service.gate.State())
	}
}

func assertConcurrentFallbackIndex(t *testing.T, fixture *fallbackPublicationFixture) {
	t.Helper()
	fixture.objectMu.Lock()
	defer fixture.objectMu.Unlock()
	if len(fixture.fallbackIndex) == 0 || !bytes.Contains(fixture.fallbackIndex, []byte(fixture.manifestDigest)) || !bytes.Contains(fixture.fallbackIndex, []byte(fixture.concurrentDigest)) {
		t.Fatalf("fallback index did not preserve both referrers after conflict retry: %s", fixture.fallbackIndex)
	}
	if fixture.fallbackGets != 3 || fixture.fallbackPuts != 2 {
		t.Fatalf("fallback index operations = GET %d PUT %d, want read/conflict/re-read/write/read", fixture.fallbackGets, fixture.fallbackPuts)
	}
	if len(fixture.leaseProbeFailures) != 0 {
		t.Fatalf("fallback publication lease checks failed: %v", fixture.leaseProbeFailures)
	}
}
