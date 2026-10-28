package app

import (
	"errors"
	"registry-hub/internal/tasks"
)

type solidificationFailureStage struct {
	name      string
	sessionID string
	cause     error
}

func (e *solidificationFailureStage) Error() string { return e.cause.Error() }
func (e *solidificationFailureStage) Unwrap() error { return e.cause }

func withSolidificationFailureStage(err error, stage string, sessionIDs ...string) error {
	var failure *tasks.Failure
	if !errors.As(err, &failure) {
		return err
	}
	copy := *failure
	stageFailure := &solidificationFailureStage{name: stage, cause: failure.Err}
	if len(sessionIDs) > 0 {
		stageFailure.sessionID = sessionIDs[0]
	}
	copy.Err = stageFailure
	return &copy
}
