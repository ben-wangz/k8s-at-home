package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"time"
)

func (s *solidifier) cacheHas(ctx context.Context, task *tasks.Task, reference contentRef) (bool, error) {
	inspection, err := s.cache.Inspect(ctx, task.Authority, task.Repository, reference.digest, reference.mediaType)
	if err != nil {
		return false, err
	}
	if inspection.Present || inspection.Unknown {
		if reference.size >= 0 && inspection.Size != reference.size {
			return false, errors.New("cached object size differs from its descriptor")
		}
		return true, nil
	}
	if reference.mediaType != "" {
		return false, nil
	}
	inspection, err = s.cache.InspectAny(ctx, task.Authority, task.Repository, reference.digest, "application/octet-stream")
	if err != nil {
		return false, err
	}
	if !inspection.Present && !inspection.Unknown {
		return false, nil
	}
	if reference.size >= 0 && inspection.Size != reference.size {
		return false, errors.New("cached object size differs from its descriptor")
	}
	return true, nil
}

func (s *solidifier) retryUpstream(ctx context.Context, authority, repository, requestPath string, query url.Values, headers http.Header) (*http.Response, error) {
	var lastErr error
	waited := time.Duration(0)
	for attempt := 0; attempt < maxUpstreamRetryAttempts; attempt++ {
		var retryAfter string
		response, err := s.upstream.Do(ctx, authority, repository, http.MethodGet, requestPath, query, headers)
		if err != nil {
			lastErr = err
			if !upstream.Retryable(err) || attempt == maxUpstreamRetryAttempts-1 {
				return nil, lastErr
			}
		} else if response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusServiceUnavailable && response.StatusCode != http.StatusGatewayTimeout {
			return response, nil
		} else {
			retryAfter = response.Header.Get("Retry-After")
			lastErr = fmt.Errorf("upstream returned status %d", response.StatusCode)
			_ = response.Body.Close()
			if attempt == maxUpstreamRetryAttempts-1 {
				return nil, lastErr
			}
		}
		delay, ok := upstreamRetryDelay(retryAfter, attempt, waited)
		if !ok {
			return nil, lastErr
		}
		if _, hasRetryAfter := parseRetryAfter(retryAfter); !hasRetryAfter {
			waited += delay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func renewLease(ctx context.Context, cancel context.CancelFunc, manager *lease.Manager, current lease.Lease, done chan<- error) {
	renewLeaseWithInterval(ctx, cancel, manager, current, objectLeaseRenewInterval, done)
}
