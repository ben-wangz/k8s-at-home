package runner

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// NewOwnerUUID returns a random UUID-v4-style hex string used only for
// staging and ownership identity, never for the backup ID itself.
func NewOwnerUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString(make([]byte, 16))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// DefaultClock is the production wall clock.
func DefaultClock() time.Time { return time.Now() }

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
