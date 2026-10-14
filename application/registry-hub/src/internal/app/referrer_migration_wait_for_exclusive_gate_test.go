package app

import (
	"testing"
	"time"
)

func waitForExclusiveGate(t *testing.T, gate *writeGate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gate.State().Exclusive {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("migration did not acquire exclusive write admission")
}
