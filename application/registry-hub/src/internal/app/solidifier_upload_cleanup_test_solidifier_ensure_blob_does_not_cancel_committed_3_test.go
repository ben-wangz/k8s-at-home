package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestSolidifierEnsureBlobDoesNotCancelCommittedBlobAfterCheckpointCancellation(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{cancelBeforeCheckpoint: true})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	if !errors.Is(err, tasks.ErrCancelled) {
		t.Fatalf("ensureBlob() error = %v, want cancellation during checkpoint", err)
	}
	if fixture.registry.setupErr != nil {
		t.Fatalf("cancel task after commit: %v", fixture.registry.setupErr)
	}
	if fixture.registry.deleteRequests != 0 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("confirmed blob was cancelled or lost: DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
	}
}

func TestSolidifierEnsureBlobHandlesCommitDigestFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		options solidifierUploadTestOptions
	}{
		{name: "missing digest", options: solidifierUploadTestOptions{omitCommitDigest: true}},
		{name: "wrong digest", options: solidifierUploadTestOptions{commitDigest: "sha256:wrong"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSolidifierUploadTestFixture(t, test.options)
			err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
			var failure *tasks.Failure
			if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
				t.Fatalf("ensureBlob() error = %#v, want temporary distribution failure", err)
			}
			if fixture.registry.deleteRequests != 1 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
				t.Fatalf("DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
			}
		})
	}
}

func TestSolidifierEnsureBlobFindsBlobAfterCommitResponseLoss(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{dropCommitResponse: true})
	taskLease := fixture.acquireTaskLease(t)
	err := fixture.runBlob(context.Background(), taskLease)
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() first attempt error = %#v, want temporary distribution failure", err)
	}
	if fixture.registry.deleteRequests != 1 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("lost commit response left DELETEs=%d active=%t committed=%q", fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
	}
	if err := fixture.runBlob(context.Background(), taskLease); err != nil {
		t.Fatalf("ensureBlob() retry for committed blob: %v", err)
	}
	if fixture.registry.postRequests != 1 || fixture.registry.putRequests != 1 || fixture.registry.headRequests != 2 {
		t.Fatalf("POSTs=%d PUTs=%d HEADs=%d, want one upload and an existing-blob retry", fixture.registry.postRequests, fixture.registry.putRequests, fixture.registry.headRequests)
	}
}

func TestSolidifierEnsureBlobPreservesPrimaryErrorWhenCancellationFails(t *testing.T) {
	var output bytes.Buffer
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, cleanupStatus: http.StatusInternalServerError, logOutput: &output,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "distribution" || failure.Permanent {
		t.Fatalf("ensureBlob() error = %#v, want original temporary distribution failure", err)
	}
	if fixture.registry.deleteRequests != 1 || !strings.Contains(output.String(), "internal upload cleanup was not confirmed") {
		t.Fatalf("cleanup DELETEs=%d log=%q", fixture.registry.deleteRequests, output.String())
	}
	if strings.Contains(output.String(), "_state") || strings.Contains(output.String(), "offset%3D0") {
		t.Fatalf("cleanup log exposed upload state: %s", output.String())
	}
}

func TestSolidifierEnsureBlobRefusesMalformedLocationWithoutOutboundCleanup(t *testing.T) {
	var output bytes.Buffer
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		invalidLocation: "https://outside.example/v2/other/blobs/uploads/session?_state=sensitive",
		logOutput:       &output,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || !failure.Permanent || failure.Class != "distribution_invalid" {
		t.Fatalf("ensureBlob() error = %#v, want permanent invalid location", err)
	}
	if fixture.registry.deleteRequests != 0 || fixture.registry.statusRequests != 0 || !strings.Contains(output.String(), "internal upload cleanup was not confirmed") {
		t.Fatalf("DELETEs=%d status GETs=%d log=%q", fixture.registry.deleteRequests, fixture.registry.statusRequests, output.String())
	}
	if strings.Contains(output.String(), "sensitive") || strings.Contains(output.String(), "outside.example") {
		t.Fatalf("cleanup log exposed the invalid location: %s", output.String())
	}
}

func TestSolidifierEnsureBlobHonorsCancelledWorkContextDuringPut(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{blockPUT: true})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	taskLease := fixture.acquireTaskLease(t)
	go func() { result <- fixture.runBlob(ctx, taskLease) }()
	select {
	case <-fixture.registry.putStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("PUT did not start")
	}
	cancel()
	close(fixture.registry.finishPUT)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("ensureBlob() succeeded after its work context was cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ensureBlob() did not finish after cancellation")
	}
	if fixture.registry.deleteRequests != 1 || fixture.registry.active {
		t.Fatalf("cleanup DELETEs=%d active=%t", fixture.registry.deleteRequests, fixture.registry.active)
	}
}

func TestSolidifierUploadRetryDoesNotAccumulateDistributionSessions(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, partialBytes: 5,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.manager.Start(ctx)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		current, err := fixture.manager.Get(ctx, fixture.task.ID)
		if err != nil {
			t.Fatalf("read task state: %v", err)
		}
		if current.Status == tasks.Solidified {
			if current.RetryCount != 1 || current.Checkpoints[fixture.digest] != "verified" {
				t.Fatalf("completed task retry/checkpoint state = %d/%q", current.RetryCount, current.Checkpoints[fixture.digest])
			}
			if fixture.registry.postRequests != 2 || fixture.registry.deleteRequests != 2 || fixture.registry.active || !bytes.Equal(fixture.registry.committedData, fixture.data) {
				t.Fatalf("POSTs=%d DELETEs=%d active=%t committed=%q", fixture.registry.postRequests, fixture.registry.deleteRequests, fixture.registry.active, fixture.registry.committedData)
			}
			return
		}
		if current.Status == tasks.RetryWait && current.RetryCount != 1 {
			t.Fatalf("retry count = %d, want exactly 1", current.RetryCount)
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := fixture.manager.Get(ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("task did not solidify after retry: state=%q retryCount=%d errorClass=%q", current.Status, current.RetryCount, current.ErrorClass)
}

func TestSolidifierRepeatedFailuresCancelEachUploadSession(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable}, partialBytes: 5,
	})
	taskLease := fixture.acquireTaskLease(t)
	for attempt := 1; attempt <= 3; attempt++ {
		err := fixture.runBlob(context.Background(), taskLease)
		var failure *tasks.Failure
		if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
			t.Fatalf("attempt %d error = %#v, want temporary distribution failure", attempt, err)
		}
		if fixture.registry.active || fixture.registry.partialBytes != 0 || fixture.registry.postRequests != attempt || fixture.registry.deleteRequests != 2*attempt {
			t.Fatalf("after attempt %d: active=%t partial=%d POSTs=%d DELETEs=%d", attempt, fixture.registry.active, fixture.registry.partialBytes, fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	}
	if err := fixture.runBlob(context.Background(), taskLease); err != nil {
		t.Fatalf("ensureBlob() successful attempt: %v", err)
	}
	if fixture.registry.active || fixture.registry.postRequests != 4 || fixture.registry.deleteRequests != 6 || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("final state: active=%t POSTs=%d DELETEs=%d committed=%q", fixture.registry.active, fixture.registry.postRequests, fixture.registry.deleteRequests, fixture.registry.committedData)
	}
}
