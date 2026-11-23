package historyretention

import (
	"testing"
	"time"
)

func TestWithinDurationAllowsTimestampPrecisionAtConfiguredTTL(t *testing.T) {
	start := time.Date(2026, time.November, 22, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		end  time.Time
		want bool
	}{
		{name: "configured TTL", end: start.Add(20 * time.Second), want: true},
		{name: "timestamp precision", end: start.Add(20*time.Second + 361*time.Nanosecond), want: true},
		{name: "below minimum", end: start.Add(18 * time.Second), want: false},
		{name: "above tolerance", end: start.Add(20*time.Second + 101*time.Millisecond), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := withinDuration(start, test.end, 19*time.Second, 20*time.Second+100*time.Millisecond); got != test.want {
				t.Fatalf("withinDuration() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestTerminalHistoryProjectionWaitTracksConfiguredRetention(t *testing.T) {
	got, err := terminalHistoryProjectionWait(int64(240))
	if err != nil || got != 210*time.Second {
		t.Fatalf("terminalHistoryProjectionWait() = %s, %v; want 3m30s, nil", got, err)
	}
	if _, err := terminalHistoryProjectionWait(int64(30)); err == nil {
		t.Fatal("terminalHistoryProjectionWait accepted a retention without a projection margin")
	}
}
