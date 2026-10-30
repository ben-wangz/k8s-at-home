package runner

import (
	"context"
	"errors"
	"testing"
)

func TestLeaseCancellationRetainsTheRenewalFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failures := make(chan error, 1)
	cause := errors.New("registration renewal failed")
	failures <- cause
	cancel()
	if err := checkLease(ctx, failures); !errors.Is(err, cause) {
		t.Fatalf("lease cancellation obscured its renewal failure: %v", err)
	}
	if err := checkLease(ctx, failures); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation was lost after consuming the cause: %v", err)
	}
}
