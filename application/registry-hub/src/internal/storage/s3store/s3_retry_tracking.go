package s3store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type retryObserverKey struct{}

type retryObserver struct{ retried atomic.Bool }

type trackingRetryer struct{ delegate aws.Retryer }

func (r trackingRetryer) IsErrorRetryable(err error) bool { return r.delegate.IsErrorRetryable(err) }
func (r trackingRetryer) MaxAttempts() int                { return r.delegate.MaxAttempts() }
func (r trackingRetryer) RetryDelay(attempt int, err error) (time.Duration, error) {
	return r.delegate.RetryDelay(attempt, err)
}
func (r trackingRetryer) GetInitialToken() func(error) error { return r.delegate.GetInitialToken() }
func (r trackingRetryer) GetAttemptToken(ctx context.Context) (func(error) error, error) {
	if retryer, ok := r.delegate.(aws.RetryerV2); ok {
		return retryer.GetAttemptToken(ctx)
	}
	return r.delegate.GetInitialToken(), nil
}
func (r trackingRetryer) GetRetryToken(ctx context.Context, err error) (func(error) error, error) {
	if observer, ok := ctx.Value(retryObserverKey{}).(*retryObserver); ok {
		observer.retried.Store(true)
	}
	return r.delegate.GetRetryToken(ctx, err)
}

func withRetryObserver(ctx context.Context) (context.Context, *retryObserver) {
	observer := &retryObserver{}
	return context.WithValue(ctx, retryObserverKey{}, observer), observer
}

func newOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
