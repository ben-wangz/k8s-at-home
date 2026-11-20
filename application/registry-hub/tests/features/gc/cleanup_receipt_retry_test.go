package gc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryCleanupReceiptUnavailable(t *testing.T) {
	t.Run("retries 503 and returns recovery", func(t *testing.T) {
		attempts := 0
		response, err := retryCleanupReceiptUnavailable(context.Background(), func(context.Context) (*http.Response, error) {
			attempts++
			status := http.StatusServiceUnavailable
			if attempts == 2 {
				status = http.StatusOK
			}
			return cleanupReceiptResponse(status), nil
		})
		if err != nil || attempts != 2 || response.StatusCode != http.StatusOK {
			t.Fatalf("response=%v attempts=%d error=%v", response, attempts, err)
		}
		response.Body.Close()
	})

	t.Run("returns other statuses without retry", func(t *testing.T) {
		attempts := 0
		response, err := retryCleanupReceiptUnavailable(context.Background(), func(context.Context) (*http.Response, error) {
			attempts++
			return cleanupReceiptResponse(http.StatusUnauthorized), nil
		})
		if err != nil || attempts != 1 || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("response=%v attempts=%d error=%v", response, attempts, err)
		}
		response.Body.Close()
	})

	t.Run("reports sustained unavailability", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		attempts := 0
		_, err := retryCleanupReceiptUnavailable(ctx, func(context.Context) (*http.Response, error) {
			attempts++
			return cleanupReceiptResponse(http.StatusServiceUnavailable), nil
		})
		if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) || attempts != 1 {
			t.Fatalf("attempts=%d error=%v", attempts, err)
		}
	})
}

func cleanupReceiptResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}
}
