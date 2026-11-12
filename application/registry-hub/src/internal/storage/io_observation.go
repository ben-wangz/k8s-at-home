package storage

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

type ioObserverKey struct{}
type ioPurposeKey struct{}
type IOObserver func(context.Context, string, int64, time.Duration)

func WithIOObserver(ctx context.Context, observe IOObserver) context.Context {
	return context.WithValue(ctx, ioObserverKey{}, observe)
}

func WithIOPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, ioPurposeKey{}, purpose)
}

func IOPurpose(ctx context.Context) string {
	purpose, _ := ctx.Value(ioPurposeKey{}).(string)
	return purpose
}

func ObserveIO(ctx context.Context, operation string, count int64, duration time.Duration) {
	if observe, _ := ctx.Value(ioObserverKey{}).(IOObserver); observe != nil {
		observe(ctx, operation, count, duration)
	}
}

func ReportIO(ctx context.Context, store Store, operation string, count int64, duration time.Duration) {
	if observe, _ := ctx.Value(ioObserverKey{}).(IOObserver); observe != nil {
		observe(ctx, operation, count, duration)
	} else if observer, ok := store.(interface {
		ObserveIO(context.Context, string, int64, time.Duration)
	}); ok {
		observer.ObserveIO(ctx, operation, count, duration)
	}
}

type CountingReader struct {
	Reader io.Reader
	Count  int64
}

func (r *CountingReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.Count += int64(n)
	return n, err
}

type ContextReader struct {
	Context context.Context
	Reader  io.Reader
}

type measuredBody struct {
	io.ReadCloser
	ctx       context.Context
	operation string
	count     atomic.Int64
	elapsed   atomic.Int64
	closed    atomic.Bool
	emitted   atomic.Bool
}

func MeasureBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	return MeasureBodyOperation(ctx, body, "read")
}

func MeasureBodyOperation(ctx context.Context, body io.ReadCloser, operation string) io.ReadCloser {
	return &measuredBody{ReadCloser: body, ctx: ctx, operation: operation}
}

func (r *measuredBody) Read(buffer []byte) (int, error) {
	started := time.Now()
	n, err := r.ReadCloser.Read(buffer)
	r.count.Add(int64(n))
	r.elapsed.Add(int64(time.Since(started)))
	if err != nil || r.closed.Load() {
		r.flush()
	}
	return n, err
}

func (r *measuredBody) flush() {
	count, elapsed := r.count.Swap(0), r.elapsed.Swap(0)
	if !r.emitted.Swap(true) || count != 0 || elapsed != 0 {
		ObserveIO(r.ctx, r.operation, count, time.Duration(elapsed))
	}
}

func (r *measuredBody) Close() error {
	err := r.ReadCloser.Close()
	r.closed.Store(true)
	r.flush()
	return err
}

func (r ContextReader) Read(buffer []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(buffer)
}
