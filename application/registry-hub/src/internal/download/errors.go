package download

import (
	"errors"
	"fmt"
)

type PersistenceError struct {
	Stage string
	Err   error
}

func (e *PersistenceError) Error() string {
	return fmt.Sprintf("download persistence failed at %s: %v", e.Stage, e.Err)
}

func (e *PersistenceError) Unwrap() error { return e.Err }

type GuardError struct{ Err error }

func (e *GuardError) Error() string { return fmt.Sprintf("download guard failed: %v", e.Err) }

func (e *GuardError) Unwrap() error { return e.Err }

func persistenceError(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *PersistenceError
	if errors.As(err, &existing) {
		return err
	}
	return &PersistenceError{Stage: stage, Err: err}
}

func guardError(err error) error {
	if err == nil {
		return nil
	}
	var existing *GuardError
	if errors.As(err, &existing) {
		return err
	}
	return &GuardError{Err: err}
}

type StatusError struct {
	Code       int
	RetryAfter string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream blob returned status %d", e.Code)
}
