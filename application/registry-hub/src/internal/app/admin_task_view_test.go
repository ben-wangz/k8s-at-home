package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestTaskDetailViewPagesGraphAndCheckpointMaps(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := tasks.NewRecords(store)
	service := &Service{records: records}
	taskID := strings.Repeat("a", 32)
	firstDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	task := tasks.Task{
		ID: taskID,
		Graph: map[string]tasks.GraphEntry{
			firstDigest:  {Digest: firstDigest, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 1},
			secondDigest: {Digest: secondDigest, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 2},
		},
		Checkpoints: map[string]string{firstDigest: "verified_local", secondDigest: "anchored"},
		GCProtected: true,
	}
	for _, digest := range []string{firstDigest, secondDigest} {
		node := tasks.GraphNode{
			RootTaskID: taskID, Role: tasks.GraphBlob, Digest: digest,
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", DeclaredSize: 1,
		}
		if err := records.SaveGraphNode(ctx, &node); err != nil {
			t.Fatal(err)
		}
	}

	first, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Graph) != 1 || first.GraphNextToken == "" || len(first.Checkpoints) != 1 || first.CheckpointsNextToken == "" || len(first.GraphNodes) != 1 || first.GraphNodesNextToken == "" {
		t.Fatalf("first detail page was not bounded: %#v", first)
	}
	if !first.GCProtectionActive {
		t.Fatal("task detail did not report active GC protection")
	}

	second, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{
		limit: 1, graphToken: first.GraphNextToken, graphNodesToken: first.GraphNodesNextToken,
		checkpointsToken: first.CheckpointsNextToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Graph) != 1 || second.GraphNextToken != "" || len(second.Checkpoints) != 1 || second.CheckpointsNextToken != "" || len(second.GraphNodes) != 1 || second.GraphNodesNextToken != "" {
		t.Fatalf("second detail page was not bounded: %#v", second)
	}
}

func TestParseAdminTaskDetailOptionsRejectsUnboundedPages(t *testing.T) {
	if _, err := parseAdminTaskDetailOptions(map[string][]string{"graphLimit": {"101"}}); err == nil {
		t.Fatal("graph limit above the maximum was accepted")
	}
	if _, err := parseAdminTaskDetailOptions(map[string][]string{"ownerLimit": {"101"}}); err == nil {
		t.Fatal("owner limit above the maximum was accepted")
	}
	if _, err := parseAdminTaskDetailOptions(map[string][]string{"ownerSessionID": {"invalid"}}); err == nil {
		t.Fatal("invalid owner session ID was accepted")
	}
	if _, err := parseAdminTaskDetailOptions(map[string][]string{"unexpected": {"value"}}); err == nil {
		t.Fatal("unsupported task detail query was accepted")
	}
}

func TestTaskDetailViewPagesProtectionOwnersAndReportsSessionStatus(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	firstOwner := mustGateTestID(t)
	secondOwner := mustGateTestID(t)
	sessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:manifest", firstOwner, 256, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := gate.AddProtectionOwner(ctx, sessionID, secondOwner); err != nil {
		t.Fatal(err)
	}
	service := &Service{records: tasks.NewRecords(store), gate: gate}
	task := tasks.Task{
		ID: firstOwner, SessionIDs: []string{sessionID}, ProtectionSessionID: sessionID, GCProtected: true,
	}
	first, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{limit: 1, ownerLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ProtectionSessions) != 1 || first.ProtectionSessions[0].OwnerCount != 2 || first.ProtectionSessions[0].ReservedBytes != 256 {
		t.Fatalf("task protection session status = %#v", first.ProtectionSessions)
	}
	if first.OwnerSessionID != sessionID || len(first.Owners) != 1 || first.OwnersNextToken == "" {
		t.Fatalf("first owner page = %#v", first)
	}
	second, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{
		limit: 1, ownerLimit: 1, ownerSessionID: sessionID, ownerToken: first.OwnersNextToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Owners) != 1 || second.OwnersNextToken != "" || second.Owners[0] == first.Owners[0] {
		t.Fatalf("second owner page = %#v, first=%#v", second, first)
	}
	if _, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{ownerSessionID: mustGateTestID(t)}); err == nil {
		t.Fatal("task detail accepted a protection session that does not belong to the task")
	}
}

func TestTaskAdminViewsIgnoreConfirmedReleasedSessionInStaleTask(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	taskID := mustGateTestID(t)
	sessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:manifest", taskID, 256, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReleaseEmptyProtection(ctx, sessionID, taskID); err != nil {
		t.Fatal(err)
	}
	service := &Service{records: tasks.NewRecords(store), gate: gate}
	task := tasks.Task{
		ID: taskID, SessionIDs: []string{sessionID}, ProtectionSessionID: sessionID,
		ReservedBytes: 256, GCProtected: true,
	}

	listView, err := service.taskAdminView(ctx, task)
	if err != nil {
		t.Fatalf("task list view with a released session: %v", err)
	}
	if listView.GCProtectionActive || listView.Task.GCProtected || listView.Task.ReservedBytes != 0 || len(listView.Task.SessionIDs) != 0 || listView.Task.ProtectionSessionID != "" || len(listView.ProtectionSessions) != 0 {
		t.Fatalf("released protection remained in task list view: %#v", listView)
	}

	detailView, err := service.taskDetailView(ctx, task, adminTaskDetailOptions{limit: 10, ownerSessionID: sessionID, ownerToken: "stale"})
	if err != nil {
		t.Fatalf("task detail view with a released owner session: %v", err)
	}
	if detailView.GCProtectionActive || detailView.OwnerSessionID != "" || len(detailView.Owners) != 0 || len(detailView.ProtectionSessions) != 0 {
		t.Fatalf("released owner session remained in task detail: %#v", detailView)
	}
}

type delayedProtectionSessionStore struct {
	storage.Store
	missingReads map[string]int
}

func (s *delayedProtectionSessionStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if remaining := s.missingReads[key]; remaining > 0 {
		s.missingReads[key] = remaining - 1
		return storage.Object{}, storage.ErrNotFound
	}
	return s.Store.Get(ctx, key)
}

func TestTaskAdminEndpointsRetryDelayedReleasedSessionReads(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 0, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.Enqueue(ctx, "registry.example", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	firstSession := mustGateTestID(t)
	secondSession := mustGateTestID(t)
	for _, session := range []struct {
		id    string
		scope string
	}{{firstSession, "manifest:first"}, {secondSession, "manifest:second"}} {
		if err := gate.AdmitProtection(ctx, session.id, session.scope, task.ID, 128, 4, 1024); err != nil {
			t.Fatal(err)
		}
		if err := gate.ReleaseEmptyProtection(ctx, session.id, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	task.SessionIDs = []string{firstSession, secondSession}
	task.ProtectionSessionID = firstSession
	task.ScopeID = "manifest:first"
	task.ReservedBytes = 256
	task.GCProtected = true
	taskData, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	taskKey := "tasks/state/" + task.ID + ".json"
	taskEntry, err := store.Head(ctx, taskKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, taskKey, bytes.NewReader(taskData), int64(len(taskData)), storage.PutOptions{IfMatch: taskEntry.Version}); err != nil {
		t.Fatal(err)
	}

	service := &Service{tasks: manager, records: tasks.NewRecords(store), gate: gate}
	setDelayedReads := func() {
		gate.store = &delayedProtectionSessionStore{Store: store, missingReads: map[string]int{
			protectionSessionKey(firstSession):  2,
			protectionSessionKey(secondSession): 3,
		}}
	}
	setDelayedReads()
	listResponse := httptest.NewRecorder()
	service.listTasks(listResponse, httptest.NewRequest(http.MethodGet, "/api/v1/tasks?limit=100", nil), ctx)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("task list status = %d, body=%s", listResponse.Code, listResponse.Body.String())
	}
	var list adminTaskListResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].GCProtectionActive || list.Items[0].Task.ReservedBytes != 0 {
		t.Fatalf("task list did not normalize delayed released sessions: %#v", list)
	}

	setDelayedReads()
	detailResponse := httptest.NewRecorder()
	service.taskOperation(detailResponse, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+task.ID, nil), ctx)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("task detail status = %d, body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail adminTaskView
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.GCProtectionActive || detail.Task.ReservedBytes != 0 || len(detail.ProtectionSessions) != 0 {
		t.Fatalf("task detail did not normalize delayed released sessions: %#v", detail)
	}
}

type unavailableProtectionSessionStore struct {
	storage.Store
}

func (s unavailableProtectionSessionStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, "system/solidification/sessions/") {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func TestTaskAdminViewPreservesProtectionSessionStorageErrors(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	taskID := mustGateTestID(t)
	sessionID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:manifest", taskID, 256, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReleaseEmptyProtection(ctx, sessionID, taskID); err != nil {
		t.Fatal(err)
	}
	gate.store = unavailableProtectionSessionStore{Store: store}
	service := &Service{records: tasks.NewRecords(store), gate: gate}
	task := tasks.Task{ID: taskID, SessionIDs: []string{sessionID}, GCProtected: true}

	if _, err := service.taskAdminView(ctx, task); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("task view error = %v, want underlying storage error", err)
	}
}
