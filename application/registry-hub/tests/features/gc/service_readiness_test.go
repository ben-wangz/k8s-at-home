package gc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
)

type readinessTransport func(*http.Request) (*http.Response, error)

func (f readinessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestWaitForRegistryService(t *testing.T) {
	requests := 0
	registry := &support.RegistryClient{Base: "http://registry.invalid", Client: &http.Client{Transport: readinessTransport(func(*http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, errors.New("connection refused")
		}
		status := http.StatusServiceUnavailable
		if requests == 3 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	if err := waitForRegistryService(context.Background(), registry); err != nil || requests != 3 {
		t.Fatalf("readiness after %d requests: %v", requests, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	registry.Client.Transport = readinessTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("unavailable") })
	if err := waitForRegistryService(ctx, registry); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
}

func TestWaitForRegistryServiceRetriesStalledAttempt(t *testing.T) {
	requests := 0
	registry := &support.RegistryClient{Base: "http://registry.invalid", Client: &http.Client{Transport: readinessTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	if err := waitForRegistryService(context.Background(), registry); err != nil || requests != 2 {
		t.Fatalf("readiness after %d requests: %v", requests, err)
	}
}
