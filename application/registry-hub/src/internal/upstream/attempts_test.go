package upstream

import (
	"context"
	"io"
	"net/http"
	"registry-hub/internal/config"
	"strings"
	"testing"
)

func TestAttemptObserverSeparatesTokenAndRegistryRequests(t *testing.T) {
	client, err := New(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var observed []HTTPAttempt
	client.SetHTTPAttemptObserver(func(_ context.Context, attempt HTTPAttempt) { observed = append(observed, attempt) })
	client.client.Transport = attemptTransport{client: client, base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request}
		body := "image"
		if request.URL.Path == "/token" {
			body = `{"token":"secret-access-token"}`
		} else if request.Header.Get("Authorization") == "" {
			response.StatusCode = http.StatusUnauthorized
			response.Header.Set("WWW-Authenticate", `Bearer realm="https://8.8.8.8/token",service="registry"`)
			body = ""
		}
		response.Body, response.ContentLength = io.NopCloser(strings.NewReader(body)), int64(len(body))
		return response, nil
	})}
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet, "/v2/team/image/manifests/latest", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	headers, consumed, token := 0, false, false
	operationID := observed[0].OperationID
	for _, attempt := range observed {
		if attempt.OperationID != operationID || attempt.ID == "" || attempt.FinishedAt.Before(attempt.StartedAt) {
			t.Fatalf("attempt identity or timing=%+v", attempt)
		}
		if attempt.Stage == "response_headers" {
			headers++
			if attempt.Number != uint64(headers) {
				t.Fatalf("HTTP sequence=%+v", attempt)
			}
		}
		if attempt.Layer == "token_http_attempt" {
			token = true
			if attempt.Number != 2 {
				t.Fatalf("token counted as a new business attempt: %+v", attempt)
			}
		}
		if attempt.Stage == "response_body" && attempt.Number == 3 {
			consumed = attempt.BodyComplete && attempt.TransportBytes == 5 && attempt.Error == nil
		}
	}
	if headers != 3 || !token || !consumed {
		t.Fatalf("attempts=%+v", observed)
	}
}

func TestAttemptObserverDoesNotCallTruncatedBodyComplete(t *testing.T) {
	client, err := New(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var observed []HTTPAttempt
	client.SetHTTPAttemptObserver(func(_ context.Context, attempt HTTPAttempt) { observed = append(observed, attempt) })
	client.client.Transport = attemptTransport{client: client, base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("short")), ContentLength: 10, Request: request}, nil
	})}
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet, "/v2/team/image/manifests/latest", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	last := observed[len(observed)-1]
	if last.BodyComplete || last.TransportBytes != 5 || last.Error != io.ErrUnexpectedEOF {
		t.Fatalf("truncated body=%+v", last)
	}
}
