package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"testing"
)

func (r *solidifierUploadTestRegistry) handleUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPut && r.options.blockPUT {
		r.mu.Lock()
		r.putRequests++
		close(r.putStarted)
		r.mu.Unlock()
		select {
		case <-request.Context().Done():
		case <-r.finishPUT:
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch request.Method {
	case http.MethodPut:
		r.putRequests++
		if index := r.putRequests - 1; index < len(r.options.putStatuses) {
			if r.options.partialBytes > 0 {
				r.partialBytes = r.options.partialBytes
				r.currentToken = fmt.Sprintf("offset=%d", r.partialBytes)
			}
			writer.WriteHeader(r.options.putStatuses[index])
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read blob failed", http.StatusBadRequest)
			return
		}
		r.committedData = body
		r.active = false
		returnedDigest := r.digest
		if r.options.commitDigest != "" {
			returnedDigest = r.options.commitDigest
		}
		if r.options.dropCommitResponse {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				r.t.Error("test server does not support hijacking the commit response")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				r.t.Errorf("hijack commit response: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		if !r.options.omitCommitDigest {
			writer.Header().Set("Docker-Content-Digest", returnedDigest)
		}
		writer.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		r.statusRequests++
		if !r.active {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Location", r.location(r.currentToken))
		writer.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		r.deleteRequests++
		r.lastCleanupToken = request.URL.Query().Get("_state")
		if r.options.blockCleanup {
			close(r.cleanupStarted)
			r.mu.Unlock()
			<-r.finishCleanup
			r.mu.Lock()
		}
		if r.options.cleanupStatus != 0 {
			writer.WriteHeader(r.options.cleanupStatus)
			return
		}
		if !r.active {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.URL.Query().Get("_state") != r.currentToken {
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		r.active = false
		r.partialBytes = 0
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.Error(writer, "unsupported upload method", http.StatusMethodNotAllowed)
	}
}

func (r *solidifierUploadTestRegistry) handleBlob(writer http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if request.Method == http.MethodHead {
		r.headRequests++
		if len(r.committedData) == 0 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(r.committedData)))
		writer.Header().Set("Docker-Content-Digest", r.digest)
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodGet {
		if r.options.verifyStatus != 0 {
			writer.WriteHeader(r.options.verifyStatus)
			return
		}
		if len(r.committedData) == 0 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if r.options.cancelBeforeCheckpoint {
			_, r.setupErr = r.manager.Cancel(context.Background(), r.taskID)
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(r.committedData)))
		writer.Header().Set("Docker-Content-Digest", r.digest)
		_, _ = writer.Write(r.committedData)
		return
	}
	http.NotFound(writer, request)
}

func (r *solidifierUploadTestRegistry) location(token string) string {
	query := url.Values{"_state": []string{token}}
	return r.uploadPath + "?" + query.Encode()
}

func (f *solidifierUploadTestFixture) runBlob(ctx context.Context, taskLease lease.Lease) error {
	return f.solidifier.ensureBlob(ctx, &f.task, taskLease, contentRef{
		digest: f.digest, mediaType: "application/vnd.oci.image.layer.v1.tar", size: int64(len(f.data)),
	})
}

func TestSolidifierEnsureBlobCancelsPartialUploadBeforeReturningFailure(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, partialBytes: 5,
	})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Permanent || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() error = %#v, want temporary distribution failure", err)
	}
	if fixture.registry.active || fixture.registry.partialBytes != 0 || fixture.registry.deleteRequests != 2 || fixture.registry.statusRequests != 1 {
		t.Fatalf("upload active=%t partial=%d DELETEs=%d status GETs=%d", fixture.registry.active, fixture.registry.partialBytes, fixture.registry.deleteRequests, fixture.registry.statusRequests)
	}
}

func TestSolidifierEnsureBlobCancelsAfterTaskCancellationBeforePut(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{cancelAfterCreate: true})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	if !errors.Is(err, tasks.ErrCancelled) {
		t.Fatalf("ensureBlob() error = %v, want task cancellation", err)
	}
	if fixture.registry.setupErr != nil {
		t.Fatalf("cancel task during upload creation: %v", fixture.registry.setupErr)
	}
	if fixture.registry.putRequests != 0 || fixture.registry.deleteRequests != 1 || fixture.registry.active {
		t.Fatalf("PUTs=%d DELETEs=%d active=%t, want no PUT and a cancelled session", fixture.registry.putRequests, fixture.registry.deleteRequests, fixture.registry.active)
	}
}

func TestSolidifierEnsureBlobKeepsConfirmedCommitAfterVerificationFailure(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{verifyStatus: http.StatusServiceUnavailable})
	err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "distribution" {
		t.Fatalf("ensureBlob() error = %#v, want verification failure", err)
	}
	if fixture.registry.deleteRequests != 0 || !bytes.Equal(fixture.registry.committedData, fixture.data) {
		t.Fatalf("confirmed blob was cancelled or lost: DELETEs=%d committed=%q", fixture.registry.deleteRequests, fixture.registry.committedData)
	}
}
