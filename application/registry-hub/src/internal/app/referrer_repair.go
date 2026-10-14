package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

const referrerRepairPrefix = "repairs/referrers/"

const (
	referrerRepairSchemaPrevious = 1
	referrerRepairSchemaCurrent  = 2
)

var errSolidificationBusy = errors.New("solidification worker slot is unavailable")

type referrerRepairIntent struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Authority      string    `json:"authority"`
	Repository     string    `json:"repository"`
	Subject        string    `json:"subject"`
	Referrer       string    `json:"referrer"`
	Operation      string    `json:"operation"`
	TaskIDs        []string  `json:"taskIDs,omitempty"`
	DeleteManifest bool      `json:"deleteManifest,omitempty"`
	Retryable      bool      `json:"retryable"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"nextAttemptAt,omitempty"`
	ErrorClass     string    `json:"errorClass,omitempty"`
	Generation     uint64    `json:"generation"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	version        string
}

type referrerPermanentError struct{ cause error }

func (e *referrerPermanentError) Error() string { return e.cause.Error() }
func (e *referrerPermanentError) Unwrap() error { return e.cause }

type referrerRepairPendingError struct{ cause error }

func (e *referrerRepairPendingError) Error() string { return e.cause.Error() }
func (e *referrerRepairPendingError) Unwrap() error { return e.cause }

type referrerStatusError struct {
	operation string
	status    int
}

func (e referrerStatusError) Error() string {
	return fmt.Sprintf("%s returned status %d", e.operation, e.status)
}

func invalidReferrer(message string) error {
	return &referrerPermanentError{cause: errors.New(message)}
}

func registryStatusError(operation string, status int) error {
	return referrerStatusError{operation: operation, status: status}
}

func referrerRetryable(err error) bool {
	if errors.Is(err, ErrNativeSubjectHeader) {
		return false
	}
	var permanent *referrerPermanentError
	if errors.As(err, &permanent) {
		return false
	}
	var status referrerStatusError
	if errors.As(err, &status) {
		return status.status == http.StatusRequestTimeout || status.status == http.StatusTooManyRequests || status.status >= 500
	}
	if errors.Is(err, lease.ErrHeld) || errors.Is(err, lease.ErrLost) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func referrerErrorClass(err error) string {
	if referrerRetryable(err) {
		return "referrer_index"
	}
	return "referrer_validation"
}

func (s *Service) beginReferrerRepair(ctx context.Context, routed route.Route, subject, referrer, operation string, deleteManifest bool, taskID string) (*referrerRepairIntent, error) {
	if !route.ValidDigest(subject) || !route.ValidDigest(referrer) || operation != "add" && operation != "remove" {
		return nil, errors.New("referrer repair identity is invalid")
	}
	authority, repository := routeStorageIdentity(routed)
	reference := referrerGenerationReference(subject, referrer)
	current, err := s.advanceGeneration(ctx, authority, repository, reference, operation == "remove")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	intent := &referrerRepairIntent{
		SchemaVersion: referrerRepairSchemaCurrent, Authority: authority, Repository: repository,
		Subject: subject, Referrer: referrer, Operation: operation, DeleteManifest: deleteManifest,
		Retryable: true, NextAttemptAt: now.Add(15 * time.Second), Generation: current.Generation, CreatedAt: now, UpdatedAt: now,
	}
	if taskID != "" {
		intent.TaskIDs = []string{taskID}
	}
	return intent, s.storeReferrerRepair(ctx, intent)
}

func mergeTaskIDs(existing, incoming []string) []string {
	result := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(result)+len(incoming))
	for _, id := range result {
		seen[id] = struct{}{}
	}
	for _, id := range incoming {
		if id == "" {
			continue
		}
		if _, found := seen[id]; !found {
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}

func (s *Service) storeReferrerRepair(ctx context.Context, intent *referrerRepairIntent) error {
	intent.SchemaVersion = referrerRepairSchemaCurrent
	key := referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer)
	for attempt := 0; attempt < 8; attempt++ {
		object, err := s.store.Get(ctx, key)
		options := storage.PutOptions{IfAbsent: errors.Is(err, storage.ErrNotFound)}
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
			_ = object.Body.Close()
			var previous referrerRepairIntent
			if readErr != nil || len(data) > 1<<20 || json.Unmarshal(data, &previous) != nil || !validReferrerRepair(previous) || previous.Authority != intent.Authority || previous.Repository != intent.Repository || previous.Subject != intent.Subject || previous.Referrer != intent.Referrer {
				return errors.New("referrer repair state is invalid")
			}
			if previous.Generation > intent.Generation {
				return storage.ErrConditionFailed
			}
			if previous.Generation == intent.Generation {
				intent.CreatedAt = previous.CreatedAt
			}
			intent.TaskIDs = mergeTaskIDs(previous.TaskIDs, intent.TaskIDs)
			options.IfMatch = object.Version
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		data, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		entry, err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return err
		}
		intent.version = entry.Version
		return nil
	}
	return storage.ErrConditionFailed
}

func (s *Service) recordReferrerRepairFailure(ctx context.Context, intent *referrerRepairIntent, cause error) error {
	if intent == nil {
		return errors.New("referrer repair intent is missing")
	}
	intent.Attempts++
	intent.Retryable = referrerRetryable(cause)
	intent.ErrorClass = referrerErrorClass(cause)
	intent.UpdatedAt = time.Now().UTC()
	if intent.Retryable {
		intent.NextAttemptAt = intent.UpdatedAt.Add(referrerRetryDelay(intent.Attempts))
	} else {
		intent.NextAttemptAt = time.Time{}
	}
	return s.updateReferrerRepair(ctx, intent)
}

func (s *Service) updateReferrerRepair(ctx context.Context, intent *referrerRepairIntent) error {
	intent.SchemaVersion = referrerRepairSchemaCurrent
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	entry, err := s.store.Put(ctx, referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: intent.version})
	if err != nil {
		return err
	}
	intent.version = entry.Version
	return nil
}

func (s *Service) clearReferrerRepair(ctx context.Context, intent *referrerRepairIntent) error {
	if intent == nil || intent.version == "" {
		return nil
	}
	err := s.store.DeleteIfMatch(ctx, referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer), intent.version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
