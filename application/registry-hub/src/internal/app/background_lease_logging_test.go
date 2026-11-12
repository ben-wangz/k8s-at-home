package app

import (
	"bytes"
	"fmt"
	"log/slog"
	"registry-hub/internal/lease"
	"testing"
)

func TestExpectedBackgroundLeaseWaitDoesNotLogWarnings(t *testing.T) {
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewTextHandler(&output, nil))}
	for count := 0; count < 10; count++ {
		service.backgroundEvent("execution", "lease_wait", fmt.Errorf("leader: %w", lease.ErrHeld))
	}
	if output.Len() != 0 {
		t.Fatalf("expected lease contention produced warnings: %s", output.String())
	}
}
