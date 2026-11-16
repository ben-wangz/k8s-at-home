package storage

import (
	"context"
	"time"
)

type HTTPAttempt struct {
	Method, RequestID, Domain, Operation, KeyClass, Reason string
	Number                                                 uint64
	StartedAt                                              time.Time
	FinishedAt                                             time.Time
	Status                                                 int
	Error                                                  error
}

type HTTPAttemptObserver func(context.Context, HTTPAttempt)
