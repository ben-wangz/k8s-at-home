package s3store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestMultipartCompletionRetryRequiresCommittedObjectReadback(t *testing.T) {
	firstCompletion := make(chan struct{})
	retryCompletion := make(chan struct{})
	finishFirstCompletion := make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishFirstCompletion) }) }
	var completionCalls atomic.Int32
	var committed atomic.Bool
	var operationID atomic.Value
	var verifiedReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			operationID.Store(request.Header.Get("x-amz-meta-registry-hub-operation"))
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>retry-upload</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("uploadId") == "retry-upload":
			_, _ = io.Copy(io.Discard, request.Body)
			writer.Header().Set("ETag", `"part-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "retry-upload":
			if completionCalls.Add(1) == 1 {
				close(firstCompletion)
				<-finishFirstCompletion
				committed.Store(true)
				writeCompletedMultipart(writer)
				return
			}
			close(retryCompletion)
			writeCompletedMultipart(writer)
		case request.Method == http.MethodHead:
			if !committed.Load() {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("ETag", `"complete-etag"`)
			writer.Header().Set("Content-Length", "17")
			writer.Header().Set("x-amz-meta-registry-hub-operation", operationID.Load().(string))
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet:
			if !committed.Load() {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			verifiedReads.Add(1)
			writer.Header().Set("ETag", `"complete-etag"`)
			writer.Header().Set("Content-Length", "17")
			writer.Header().Set("x-amz-meta-registry-hub-operation", operationID.Load().(string))
			_, _ = io.WriteString(writer, "multipart payload")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer finish()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		HTTPClient:  &http.Client{Transport: transport, Timeout: 5 * time.Second},
		Retryer: func() aws.Retryer {
			return trackingRetryer{delegate: retry.NewStandard(func(options *retry.StandardOptions) {
				options.MaxAttempts = 2
				options.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})}
		},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	store := &Store{client: client, bucket: "registry-hub", multipartThreshold: 1, partSize: 5 << 20}
	defer store.Close()
	defer transport.CloseIdleConnections()

	putResult := make(chan error, 1)
	go func() {
		_, err := store.Put(context.Background(), "test/retried-completion", strings.NewReader("multipart payload"), 17, storage.PutOptions{IfAbsent: true})
		putResult <- err
	}()

	select {
	case <-firstCompletion:
	case <-time.After(time.Second):
		t.Fatal("first multipart completion did not start")
	}
	select {
	case <-retryCompletion:
	case <-time.After(3 * time.Second):
		finish()
		t.Fatal("SDK did not retry the timed-out multipart completion")
	}
	select {
	case err := <-putResult:
		finish()
		t.Fatalf("Put returned before the original completion became visible: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	finish()
	select {
	case err := <-putResult:
		if err != nil {
			t.Fatalf("Put after the original completion became visible: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Put did not confirm the original completion")
	}
	if got := completionCalls.Load(); got != 2 {
		t.Fatalf("completion calls = %d, want two attempts", got)
	}
	if got := verifiedReads.Load(); got != 1 {
		t.Fatalf("commit verification reads = %d, want one full read", got)
	}
}

func writeCompletedMultipart(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(writer, `<CompleteMultipartUploadResult><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
}
