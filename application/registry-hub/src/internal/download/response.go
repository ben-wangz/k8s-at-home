package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errUpstreamValidatorChanged = errors.New("upstream blob validator changed during download")
var errProgressCommit = errors.New("download progress could not be committed")

func (m *Manager) acceptResponse(ctx context.Context, loaded *stateVersion, response *http.Response, start, end int64, request Request) (int64, error) {
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, errors.New("upstream blob response used an unsupported content encoding")
	}
	if response.StatusCode == http.StatusOK {
		if start != 0 {
			return 0, errors.New("upstream returned a full blob for a nonzero offset")
		}
		total := response.ContentLength
		if total < 0 {
			total = request.ExpectedSize
		}
		if total > request.MaxBytes || request.ExpectedSize >= 0 && total >= 0 && total != request.ExpectedSize {
			return 0, errors.New("upstream blob size does not match its descriptor or configured limit")
		}
		if digest := response.Header.Get("Docker-Content-Digest"); digest != "" && digest != request.Digest {
			return 0, errors.New("upstream blob digest header does not match the requested digest")
		}
		candidate := cloneState(loaded.Value)
		candidate.TotalSize = total
		candidate.ETag = strongETag(response.Header.Get("ETag"))
		if err := m.updateState(ctx, loaded, candidate, request); err != nil {
			return 0, err
		}
		return total, nil
	}
	if response.StatusCode != http.StatusPartialContent || start <= 0 {
		if response.StatusCode == http.StatusPartialContent {
			return 0, errors.New("upstream returned partial content without a Range request")
		}
		return 0, fmt.Errorf("upstream blob returned status %d", response.StatusCode)
	}
	if mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err == nil && strings.EqualFold(mediaType, "multipart/byteranges") {
		return 0, errors.New("upstream returned multipart range data for a single-range request")
	}
	if digest := response.Header.Get("Docker-Content-Digest"); digest != "" && digest != request.Digest {
		return 0, errors.New("upstream blob digest header does not match the requested digest")
	}
	first, last, total, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil || first != start || last < first || last > end || total <= last || total > request.MaxBytes {
		return 0, errors.New("upstream Content-Range does not match the requested continuation")
	}
	if request.ExpectedSize >= 0 && total != request.ExpectedSize {
		return 0, errors.New("upstream range total does not match the object descriptor")
	}
	if loaded.Value.TotalSize >= 0 && total != loaded.Value.TotalSize {
		return 0, errUpstreamValidatorChanged
	}
	if response.ContentLength >= 0 && response.ContentLength != last-first+1 {
		return 0, errors.New("upstream partial Content-Length does not match Content-Range")
	}
	if previous := loaded.Value.ETag; previous != "" {
		if current := strongETag(response.Header.Get("ETag")); current != "" && current != previous {
			return 0, errUpstreamValidatorChanged
		}
	}
	candidate := cloneState(loaded.Value)
	candidate.TotalSize = total
	if current := strongETag(response.Header.Get("ETag")); current != "" {
		candidate.ETag = current
	}
	if err := m.updateState(ctx, loaded, candidate, request); err != nil {
		return 0, err
	}
	return last - first + 1, nil
}

func parseContentRange(value string) (int64, int64, int64, error) {
	unit, remainder, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, errors.New("Content-Range unit is invalid")
	}
	interval, totalValue, found := strings.Cut(remainder, "/")
	if !found || strings.Contains(totalValue, "/") || totalValue == "*" {
		return 0, 0, 0, errors.New("Content-Range total is unknown")
	}
	firstValue, lastValue, found := strings.Cut(interval, "-")
	if !found {
		return 0, 0, 0, errors.New("Content-Range interval is invalid")
	}
	first, firstErr := strconv.ParseInt(firstValue, 10, 64)
	last, lastErr := strconv.ParseInt(lastValue, 10, 64)
	total, totalErr := strconv.ParseInt(totalValue, 10, 64)
	if firstErr != nil || lastErr != nil || totalErr != nil || first < 0 || last < first || total <= last {
		return 0, 0, 0, errors.New("Content-Range values are invalid")
	}
	return first, last, total, nil
}

func (m *Manager) copyResponse(ctx context.Context, body io.Reader, cancelBody context.CancelFunc, loaded *stateVersion, request Request, responseStart, responseLength int64) error {
	buffer := make([]byte, m.chunkSize)
	var buffered []byte
	var received int64
	flush := func(flushCtx context.Context, final bool) error {
		for len(buffered) >= len(buffer) || final && len(buffered) > 0 {
			size := len(buffer)
			if len(buffered) < size {
				size = len(buffered)
			}
			if err := m.appendChunk(flushCtx, loaded, request, loaded.Value.Offset, buffered[:size]); err != nil {
				var guardErr *GuardError
				if errors.As(err, &guardErr) || errors.Is(err, ErrQuotaExceeded) {
					return errors.Join(errProgressCommit, err)
				}
				return errors.Join(errProgressCommit, persistenceError("chunk_commit", err))
			}
			buffered = buffered[size:]
		}
		if len(buffered) == 0 {
			buffered = nil
		}
		return nil
	}
	for {
		count, readErr := m.readChunk(ctx, body, cancelBody, buffer, request.IdleTimeout)
		if count > 0 {
			received += int64(count)
			if received > request.MaxBytes-responseStart || responseLength >= 0 && received > responseLength {
				return errors.New("upstream blob response exceeds the expected length")
			}
			buffered = append(buffered, buffer[:count]...)
			if len(buffered) >= len(buffer) && readErr == nil {
				if err := flush(ctx, false); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			flushCtx := ctx
			finish := func() {}
			needsFinalState := errors.Is(readErr, io.EOF) && loaded.Value.TotalSize < 0
			if ctx.Err() != nil && (len(buffered) > 0 || needsFinalState) {
				finalCtx, finalize, finalizeErr, allowed := m.salvageContext(ctx, request)
				if finalizeErr != nil {
					return errors.Join(readErr, finalizeErr)
				}
				if !allowed {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return readErr
				}
				flushCtx = finalCtx
				finish = finalize
			}
			flushErr := flush(flushCtx, true)
			defer finish()
			if flushErr != nil {
				return errors.Join(readErr, flushErr)
			}
			if errors.Is(readErr, io.EOF) {
				if responseLength >= 0 && received != responseLength {
					return errors.New("upstream body ended before its declared content length")
				}
				if loaded.Value.TotalSize < 0 {
					candidate := cloneState(loaded.Value)
					candidate.TotalSize = loaded.Value.Offset
					if err := m.updateState(flushCtx, loaded, candidate, request); err != nil {
						return err
					}
				}
				if loaded.Value.Offset != loaded.Value.TotalSize || request.ExpectedSize >= 0 && loaded.Value.Offset != request.ExpectedSize {
					return io.ErrUnexpectedEOF
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return readErr
		}
	}
}

func (m *Manager) salvageContext(ctx context.Context, request Request) (context.Context, func(), error, bool) {
	if request.LifecycleContext == nil || request.Guard == nil || request.LifecycleContext.Err() != nil {
		return nil, nil, nil, false
	}
	timeout := request.FinalizeTimeout
	if timeout <= 0 {
		timeout = defaultFinalizeTimeout
	}
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	stopLifecycle := context.AfterFunc(request.LifecycleContext, cancel)
	if request.LifecycleContext.Err() != nil {
		stopLifecycle()
		cancel()
		return nil, nil, nil, false
	}
	if err := request.Guard(finalCtx); err != nil {
		stopLifecycle()
		cancel()
		return nil, nil, guardError(err), false
	}
	finish := func() {
		stopLifecycle()
		cancel()
	}
	return finalCtx, finish, nil, true
}

func (m *Manager) readChunk(ctx context.Context, body io.Reader, cancelBody context.CancelFunc, buffer []byte, idleTimeout time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if idleTimeout <= 0 {
		return body.Read(buffer)
	}
	timer := time.AfterFunc(idleTimeout, cancelBody)
	count, err := body.Read(buffer)
	timer.Stop()
	if ctx.Err() == nil && err != nil && errors.Is(err, context.Canceled) {
		return count, context.DeadlineExceeded
	}
	return count, err
}
