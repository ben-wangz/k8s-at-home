package s3store

import (
	"net/http"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type attemptTransport struct {
	base     http.RoundTripper
	domain   string
	observer atomic.Pointer[storage.HTTPAttemptObserver]
}

func (t *attemptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now().UTC()
	if request.Body != nil {
		request.Body = storage.MeasureBodyOperation(request.Context(), request.Body, "s3_write")
	}
	response, err := t.base.RoundTrip(request)
	if response != nil && response.Body != nil {
		response.Body = storage.MeasureBodyOperation(request.Context(), response.Body, "s3_read")
	}
	observer := t.observer.Load()
	if observer != nil {
		attribution := storage.CallAttributionFromContext(request.Context())
		attempt := storage.HTTPAttempt{Method: request.Method, Domain: t.domain, Operation: attribution.Operation, KeyClass: attribution.KeyClass, Reason: attribution.Reason, StartedAt: started, FinishedAt: time.Now().UTC(), Error: err}
		for _, field := range strings.Split(request.Header.Get("Amz-Sdk-Request"), ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && key == "attempt" {
				attempt.Number, _ = strconv.ParseUint(value, 10, 64)
			}
		}
		if response != nil {
			attempt.Status = response.StatusCode
			attempt.RequestID = response.Header.Get("X-Amz-Request-Id")
		}
		(*observer)(request.Context(), attempt)
	}
	return response, err
}

func (s *Store) SetHTTPAttemptObserver(observer storage.HTTPAttemptObserver) {
	if s.attempts == nil {
		return
	}
	if observer == nil {
		s.attempts.observer.Store(nil)
	} else {
		s.attempts.observer.Store(&observer)
	}
}
