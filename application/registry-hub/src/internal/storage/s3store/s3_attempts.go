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
	observer atomic.Pointer[storage.HTTPAttemptObserver]
}

func (t *attemptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now().UTC()
	response, err := t.base.RoundTrip(request)
	observer := t.observer.Load()
	if observer != nil {
		attempt := storage.HTTPAttempt{Method: request.Method, StartedAt: started, FinishedAt: time.Now().UTC(), Error: err}
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
