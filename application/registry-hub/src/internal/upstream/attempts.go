package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"registry-hub/internal/route"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type HTTPAttempt struct {
	ID, OperationID, Authority, Repository, Digest string
	Layer, Stage, Method                           string
	Number                                         uint64
	StartedAt, FinishedAt                          time.Time
	Status                                         int
	TransportBytes                                 int64
	ExpectedBytes                                  *int64
	BodyComplete                                   bool
	Error                                          error
}

type HTTPAttemptObserver func(context.Context, HTTPAttempt)
type attemptScopeKey struct{}
type tokenAttemptKey struct{}

type attemptScope struct {
	ID, Authority, Repository, Digest string
	Number                            atomic.Uint64
}

func (c *Client) SetHTTPAttemptObserver(observer HTTPAttemptObserver) {
	c.attemptObserver.Store(&observer)
}

func (c *Client) attemptContext(ctx context.Context, authority, repository, path string) context.Context {
	if c.attemptObserver.Load() == nil {
		return ctx
	}
	var random [16]byte
	_, _ = rand.Read(random[:])
	scope := &attemptScope{ID: hex.EncodeToString(random[:]), Authority: authority, Repository: repository}
	if tail := path[strings.LastIndex(path, "/")+1:]; route.ValidDigest(tail) {
		scope.Digest = tail
	}
	return context.WithValue(ctx, attemptScopeKey{}, scope)
}

type attemptTransport struct {
	base   http.RoundTripper
	client *Client
}

func (t attemptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	observer := t.client.attemptObserver.Load()
	scope, ok := request.Context().Value(attemptScopeKey{}).(*attemptScope)
	if observer == nil || *observer == nil || !ok {
		return t.base.RoundTrip(request)
	}
	number := scope.Number.Add(1)
	attempt := HTTPAttempt{ID: scope.ID + "/" + time.Now().UTC().Format(time.RFC3339Nano), OperationID: scope.ID, Authority: scope.Authority, Repository: scope.Repository, Digest: scope.Digest, Layer: "upstream_http_attempt", Stage: "response_headers", Method: request.Method, Number: number, StartedAt: time.Now().UTC()}
	if request.Context().Value(tokenAttemptKey{}) != nil {
		attempt.Layer = "token_http_attempt"
	}
	response, err := t.base.RoundTrip(request)
	attempt.FinishedAt, attempt.Error = time.Now().UTC(), err
	if response != nil {
		attempt.Status = response.StatusCode
		if response.ContentLength >= 0 {
			size := response.ContentLength
			if request.Method == http.MethodHead || response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified {
				size = 0
			}
			attempt.ExpectedBytes = &size
		}
	}
	(*observer)(request.Context(), attempt)
	if err == nil && response != nil && response.Body != nil {
		response.Body = &attemptBody{ReadCloser: response.Body, context: request.Context(), attempt: attempt, observer: *observer}
	}
	return response, err
}

type attemptBody struct {
	io.ReadCloser
	context  context.Context
	attempt  HTTPAttempt
	observer HTTPAttemptObserver
	bytes    atomic.Int64
	finished sync.Once
}

func (b *attemptBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	b.bytes.Add(int64(n))
	if err != nil {
		b.finish(err == io.EOF, err)
	}
	return n, err
}

func (b *attemptBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(false, err)
	return err
}

func (b *attemptBody) finish(complete bool, err error) {
	b.finished.Do(func() {
		attempt := b.attempt
		attempt.Stage, attempt.FinishedAt = "response_body", time.Now().UTC()
		attempt.TransportBytes, attempt.BodyComplete = b.bytes.Load(), complete
		if err == io.EOF {
			err = nil
		}
		if complete && attempt.ExpectedBytes != nil && attempt.TransportBytes != *attempt.ExpectedBytes {
			attempt.BodyComplete, err = false, io.ErrUnexpectedEOF
		}
		attempt.Error = err
		b.observer(b.context, attempt)
	})
}
