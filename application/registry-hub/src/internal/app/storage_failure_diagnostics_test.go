package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/tasks"
)

type testS3RequestError struct{}

func (testS3RequestError) Error() string       { return "storage unavailable" }
func (testS3RequestError) S3RequestID() string { return "rustfs-request-123" }

func TestObservedStoreLogsSafeStorageCorrelation(t *testing.T) {
	var output bytes.Buffer
	store := &observedStore{
		metrics: newMetrics(),
		logger:  slog.New(slog.NewJSONHandler(&output, nil)),
	}
	ctx := context.WithValue(context.Background(), requestMetaKey{}, &requestMeta{requestID: "hub-request-456"})
	ctx = context.WithValue(ctx, storageOperationIDKey{}, "solidification-task-789")
	store.record(ctx, "put", testS3RequestError{}, time.Now())

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"request_id":    "hub-request-456",
		"operation_id":  "solidification-task-789",
		"s3_request_id": "rustfs-request-123",
		"error_class":   "error",
		"operation":     "put",
		"msg":           "storage operation failed",
	} {
		if got, _ := record[key].(string); got != want {
			t.Errorf("log %s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(output.String(), "secret") {
		t.Fatalf("diagnostic log included sensitive details: %s", output.String())
	}
}

func TestTaskStateFailureIncludesStableStage(t *testing.T) {
	err := withSolidificationFailureStage(temporary("task_state", errors.New("write unavailable")), "task_progress_commit")
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "task_state" {
		t.Fatalf("failure = %#v, want task_state", failure)
	}
	var stage *solidificationFailureStage
	if !errors.As(failure.Err, &stage) || stage.name != "task_progress_commit" {
		t.Fatalf("failure stage = %#v, want task_progress_commit", stage)
	}
}
