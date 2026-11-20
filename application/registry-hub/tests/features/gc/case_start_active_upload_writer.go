package gc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func startActiveUploadWriter(ctx context.Context, registry *support.RegistryClient, repository, runID string) (*activeUploadWriter, error) {
	payload := []byte("registry-hub-gc-active-writer:" + runID)
	created, endpoint, err := registry.CreateUpload(ctx, repository, nil)
	if err != nil {
		return nil, err
	}
	created.Body.Close()
	reader, writer := io.Pipe()
	result := make(chan activeUploadResult, 1)
	go func() {
		response, requestErr := registry.Request(ctx, http.MethodPatch, endpoint, reader, map[string]string{
			"Content-Type":  "application/octet-stream",
			"Content-Range": fmt.Sprintf("0-%d", len(payload)-1),
		})
		if requestErr != nil {
			result <- activeUploadResult{err: requestErr}
			return
		}
		status := response.StatusCode
		response.Body.Close()
		result <- activeUploadResult{status: status}
	}()
	first := len(payload) / 2
	if _, err := writer.Write(payload[:first]); err != nil {
		writer.CloseWithError(err)
		return nil, fmt.Errorf("start active Registry upload: %w", err)
	}
	return &activeUploadWriter{writer: writer, result: result, last: payload[first:]}, nil
}

func waitForActiveUploadAdmission(ctx context.Context, environment testkit.CaseContext, result <-chan activeUploadResult) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case writeResult := <-result:
			return fmt.Errorf("active upload completed before GC started: status=%d err=%v", writeResult.status, writeResult.err)
		default:
		}
		status, err := gcStatus(ctx, environment)
		if err != nil {
			return err
		}
		if testkit.Int(status["activeWrites"]) > 0 {
			return nil
		}
		select {
		case writeResult := <-result:
			return fmt.Errorf("active upload completed before GC started: status=%d err=%v", writeResult.status, writeResult.err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Registry did not admit the active upload within 15 seconds")
		case <-poll.C:
		}
	}
}

func verifyGCWaitsForActiveWriter(ctx context.Context, environment testkit.CaseContext, result <-chan activeUploadResult) error {
	deadline := time.NewTimer(750 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case writeResult := <-result:
			return fmt.Errorf("active upload completed before its body was released: status=%d err=%v", writeResult.status, writeResult.err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			status, err := gcStatus(ctx, environment)
			if err != nil {
				return err
			}
			run := testkit.Map(status["run"])
			gate := testkit.Map(status["gate"])
			if testkit.String(run["state"]) != "draining" || testkit.String(gate["state"]) != "draining" || testkit.String(run["jobName"]) != "" {
				return fmt.Errorf("GC advanced before the accepted upload completed: run=%q gate=%q job=%q",
					testkit.String(run["state"]), testkit.String(gate["state"]), testkit.String(run["jobName"]))
			}
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func finishActiveUploadWriter(active *activeUploadWriter) (int, error) {
	if _, err := active.writer.Write(active.last); err != nil {
		active.writer.CloseWithError(err)
		return 0, fmt.Errorf("finish active Registry upload body: %w", err)
	}
	if err := active.writer.Close(); err != nil {
		return 0, err
	}
	select {
	case result := <-active.result:
		if result.err != nil {
			return 0, result.err
		}
		if result.status != http.StatusAccepted {
			return result.status, fmt.Errorf("accepted active upload returned %d, want 202", result.status)
		}
		return result.status, nil
	case <-time.After(30 * time.Second):
		return 0, fmt.Errorf("active Registry upload did not finish after its body was released")
	}
}

func gcStatus(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
	if err != nil {
		return nil, err
	}
	var status map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
		return nil, err
	}
	return status, nil
}
