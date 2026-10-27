package app

import (
	"context"
	"strings"
	"testing"

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
