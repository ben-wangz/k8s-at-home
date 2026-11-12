package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func solidificationLogEntries(t *testing.T, output *bytes.Buffer, message string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var entries []map[string]any
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == message {
			entries = append(entries, entry)
		}
	}
}

func TestSolidificationBlobPerformanceDistinguishesUploadAndReuse(t *testing.T) {
	for _, test := range []struct {
		name        string
		reuse       bool
		observation bool
	}{
		{name: "upload"},
		{name: "reuse", reuse: true},
		{name: "reuse verified evidence", reuse: true, observation: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			f := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{logOutput: &output})
			if test.reuse {
				f.registry.committedData = append([]byte(nil), f.data...)
			}
			taskLease := f.acquireTaskLease(t)
			if test.observation {
				if err := f.runBlob(context.Background(), taskLease); err != nil {
					t.Fatal(err)
				}
				output.Reset()
			}
			f.task.AttemptID = "performance-attempt"
			if err := f.runBlob(context.Background(), taskLease); err != nil {
				t.Fatal(err)
			}
			entries := solidificationLogEntries(t, &output, "solidification blob verified")
			if len(entries) != 1 {
				t.Fatalf("verified logs = %d, want one: %s", len(entries), output.String())
			}
			entry := entries[0]
			if entry["task_id"] != f.task.ID || entry["attempt_id"] != "performance-attempt" || entry["digest"] != f.digest || entry["size_bytes"] != float64(len(f.data)) {
				t.Fatalf("missing content/attempt identity: %v", entry)
			}
			if entry["distribution_readback_bytes"] != float64(0) || entry["execution_seconds"].(float64) <= 0 || entry["distribution_verify_seconds"].(float64) <= 0 {
				t.Fatalf("missing verification measurements: %v", entry)
			}
			if test.reuse {
				if entry["source"] != "distribution" || entry["uploaded_bytes"] != float64(0) || entry["upload_seconds"] != float64(0) || entry["cache_open_seconds"] != float64(0) || f.registry.postRequests != 0 {
					t.Fatalf("reuse reported an upload: %v", entry)
				}
			} else if entry["source"] != "cache" || entry["uploaded_bytes"] != float64(len(f.data)) || entry["upload_seconds"].(float64) <= 0 || entry["cache_open_seconds"].(float64) <= 0 {
				t.Fatalf("missing upload measurements: %v", entry)
			}
			if strings.Contains(output.String(), "_state") || strings.Contains(output.String(), "offset%3D0") {
				t.Fatalf("performance log exposed upload credentials: %s", output.String())
			}
		})
	}
}

func TestSolidificationUploadFailureLogsAttemptCostWithoutReadback(t *testing.T) {
	var output bytes.Buffer
	f := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{logOutput: &output, omitCommitDigest: true})
	f.solidifier.service.solidificationSlots = make(chan struct{}, 1)
	if err := f.solidifier.service.records.LinkBlobDemand(context.Background(), f.task.ID, f.task.ID); err != nil {
		t.Fatal(err)
	}
	err := f.solidifier.Process(context.Background(), &f.task, f.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "distribution" {
		t.Fatalf("Process = %v, want upload confirmation failure", err)
	}
	entries := solidificationLogEntries(t, &output, "solidification failed")
	if len(entries) != 1 || len(solidificationLogEntries(t, &output, "solidification blob verified")) != 0 {
		t.Fatalf("want one failure without success: %s", output.String())
	}
	entry := entries[0]
	if entry["level"] != "WARN" || entry["uploaded_bytes"] != float64(len(f.data)) || entry["upload_seconds"].(float64) <= 0 || entry["distribution_verify_seconds"].(float64) <= 0 || entry["distribution_readback_bytes"] != float64(0) {
		t.Fatalf("failure lost attempt costs: %v", entry)
	}
}
