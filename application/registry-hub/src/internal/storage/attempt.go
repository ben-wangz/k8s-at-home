package storage

import (
	"context"
	"time"
)

type HTTPAttempt struct {
	Method, RequestID string
	Number            uint64
	StartedAt         time.Time
	FinishedAt        time.Time
	Status            int
	Error             error
}

type HTTPAttemptObserver func(context.Context, HTTPAttempt)
