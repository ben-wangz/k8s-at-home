package download

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (m *Manager) Fetch(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	if request.TotalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.TotalTimeout)
		defer cancel()
	}
	loaded, err := m.loadOrCreate(ctx, request)
	if err != nil {
		return Result{}, err
	}
	result := Result{ResumedFrom: loaded.Value.Offset, ETag: loaded.Value.ETag}
	if loaded.Value.Offset > 0 && request.Observe != nil {
		request.Observe("download_resume")
	}
	noProgressAttempts := 0
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if loaded.Value.TotalSize >= 0 && loaded.Value.Offset == loaded.Value.TotalSize {
			result.Size = loaded.Value.Offset
			return result, nil
		}
		if loaded.Value.RangeDisabled && loaded.Value.Offset > 0 {
			if loaded.Value.Fallbacks >= 3 {
				return Result{}, errors.New("upstream repeatedly failed without Range support")
			}
			loaded, err = m.reset(ctx, loaded, request, true)
			if err != nil {
				return Result{}, err
			}
		}
		start := loaded.Value.Offset
		end := start + m.chunkSize - 1
		if loaded.Value.TotalSize >= 0 && end >= loaded.Value.TotalSize {
			end = loaded.Value.TotalSize - 1
		}
		response, cancel, requestErr := request.Source(ctx, start, end, loaded.Value.ETag)
		if requestErr != nil {
			if !m.retryable(ctx, request, requestErr) || noProgressAttempts >= maxNoProgressRetries {
				return Result{}, requestErr
			}
			noProgressAttempts++
			if err := m.waitRetry(ctx, request, "", noProgressAttempts-1); err != nil {
				return Result{}, errors.Join(requestErr, err)
			}
			continue
		}
		if response == nil || response.Body == nil {
			if cancel != nil {
				cancel()
			}
			return Result{}, errors.New("upstream returned an empty response")
		}
		if cancel == nil {
			_ = response.Body.Close()
			return Result{}, errors.New("upstream source returned no cancellation function")
		}
		result.MediaType = response.Header.Get("Content-Type")
		if response.StatusCode == http.StatusRequestedRangeNotSatisfiable && start > 0 {
			_ = response.Body.Close()
			cancel()
			if loaded.Value.ExpectedSize == start || loaded.Value.TotalSize == start {
				candidate := loaded.Value
				candidate.TotalSize = start
				result.Size = start
				updated, commitErr := m.commitState(ctx, loaded, candidate, request)
				if commitErr != nil {
					return Result{}, commitErr
				}
				loaded = updated
				return result, nil
			}
			if loaded.Value.Fallbacks > 0 {
				return Result{}, errors.New("upstream repeatedly rejected a resumable range")
			}
			loaded, err = m.reset(ctx, loaded, request, false)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		if retryableStatus(response.StatusCode) {
			statusErr := &StatusError{Code: response.StatusCode, RetryAfter: response.Header.Get("Retry-After")}
			_ = response.Body.Close()
			cancel()
			if noProgressAttempts >= maxNoProgressRetries {
				return Result{}, statusErr
			}
			noProgressAttempts++
			if err := m.waitRetry(ctx, request, statusErr.RetryAfter, noProgressAttempts-1); err != nil {
				return Result{}, errors.Join(statusErr, err)
			}
			continue
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
			_ = response.Body.Close()
			cancel()
			return Result{}, &StatusError{Code: response.StatusCode, RetryAfter: response.Header.Get("Retry-After")}
		}
		if response.StatusCode == http.StatusOK && start > 0 {
			loaded, err = m.reset(ctx, loaded, request, true)
			if err != nil {
				_ = response.Body.Close()
				cancel()
				return Result{}, err
			}
			start = 0
			end = m.chunkSize - 1
		}
		responseLength, err := m.acceptResponse(ctx, &loaded, response, start, end, request)
		if err != nil {
			_ = response.Body.Close()
			cancel()
			if errors.Is(err, errUpstreamValidatorChanged) && loaded.Value.Fallbacks == 0 {
				loaded, err = m.reset(ctx, loaded, request, false)
				if err != nil {
					return Result{}, err
				}
				continue
			}
			return Result{}, err
		}
		var attemptTimer *time.Timer
		if response.StatusCode == http.StatusPartialContent && request.AttemptLimit > 0 {
			attemptTimer = time.AfterFunc(request.AttemptLimit, cancel)
		}
		readErr := m.copyResponse(ctx, response.Body, cancel, &loaded, request, start, responseLength)
		if attemptTimer != nil {
			attemptTimer.Stop()
		}
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			if !m.retryable(ctx, request, readErr) {
				return Result{}, readErr
			}
			if loaded.Value.Offset > start {
				noProgressAttempts = 0
			} else if noProgressAttempts >= maxNoProgressRetries {
				return Result{}, readErr
			} else {
				noProgressAttempts++
				if err := m.waitRetry(ctx, request, "", noProgressAttempts-1); err != nil {
					return Result{}, errors.Join(readErr, err)
				}
			}
			continue
		}
		if loaded.Value.Offset > start {
			noProgressAttempts = 0
		}
	}
}
