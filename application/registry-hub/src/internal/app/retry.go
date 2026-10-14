package app

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxUpstreamRetryAttempts = 3
	maxUpstreamRetryWait     = 15 * time.Second
	maxRetryAfterDelay       = 60 * time.Second
)

// upstreamRetryDelay applies the default backoff budget while honoring a valid
// Retry-After independently, subject to the caller's request deadline.
func upstreamRetryDelay(retryAfter string, attempt int, waited time.Duration) (time.Duration, bool) {
	if value, ok := parseRetryAfter(retryAfter); ok {
		if value > maxRetryAfterDelay {
			value = maxRetryAfterDelay
		}
		return value, true
	}
	delay := time.Second << attempt
	remaining := maxUpstreamRetryWait - waited
	if remaining <= 0 {
		return 0, false
	}
	if delay > remaining {
		delay = remaining
	}
	return delay, true
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(maxRetryAfterDelay/time.Second) {
			return maxRetryAfterDelay, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if when.After(time.Now().Add(maxRetryAfterDelay)) {
		return maxRetryAfterDelay, true
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}
