package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
)

const (
	generationSchemaPrevious = 1
	generationSchemaCurrent  = 2
)

type generationRecord struct {
	SchemaVersion int       `json:"schemaVersion"`
	Authority     string    `json:"authority"`
	Repository    string    `json:"repository"`
	Reference     string    `json:"reference"`
	Generation    uint64    `json:"generation"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Tombstone     bool      `json:"tombstone"`
	version       string
}

func (s *Service) generation(ctx context.Context, authority, repository, reference string) (generationRecord, error) {
	key := generationKey(authority, repository, reference)
	for attempt := 0; attempt < 16; attempt++ {
		object, err := s.store.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return generationRecord{SchemaVersion: generationSchemaCurrent, Authority: authority, Repository: repository, Reference: reference}, nil
		}
		if err != nil {
			return generationRecord{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil || len(data) > 1<<20 {
			return generationRecord{}, errors.New("generation state is invalid")
		}
		var record generationRecord
		if err := json.Unmarshal(data, &record); err != nil || record.Authority != authority || record.Repository != repository || record.Reference != reference {
			return generationRecord{}, errors.New("generation state identity is invalid")
		}
		switch record.SchemaVersion {
		case generationSchemaCurrent:
			record.version = object.Version
			return record, nil
		case generationSchemaPrevious:
			record.SchemaVersion = generationSchemaCurrent
			updated, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				return generationRecord{}, marshalErr
			}
			entry, putErr := s.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return generationRecord{}, putErr
			}
			record.version = entry.Version
			return record, nil
		default:
			return generationRecord{}, errors.New("generation state identity is invalid")
		}
	}
	return generationRecord{}, storage.ErrUnavailable
}

func (s *Service) advanceGeneration(ctx context.Context, authority, repository, reference string, tombstone bool) (generationRecord, error) {
	key := generationKey(authority, repository, reference)
	for attempt := 0; attempt < 8; attempt++ {
		current, err := s.generation(ctx, authority, repository, reference)
		if err != nil {
			return generationRecord{}, err
		}
		current.Generation++
		current.Tombstone = tombstone
		current.UpdatedAt = time.Now().UTC()
		data, err := json.Marshal(current)
		if err != nil {
			return generationRecord{}, err
		}
		options := storage.PutOptions{IfAbsent: current.version == ""}
		if current.version != "" {
			options.IfMatch = current.version
		}
		entry, err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return generationRecord{}, err
		}
		current.version = entry.Version
		return current, nil
	}
	return generationRecord{}, storage.ErrConditionFailed
}

func (s *Service) checkTaskGeneration(ctx context.Context, task *tasks.Task) error {
	current, err := s.generation(ctx, task.Authority, task.Repository, task.Reference)
	if err != nil {
		return fmt.Errorf("read task generation: %w", err)
	}
	if current.Generation != task.Generation || current.Tombstone {
		return errors.New("task generation was superseded")
	}
	return nil
}

func (s *Service) checkContentGeneration(ctx context.Context, authority, repository, digest string) error {
	record, err := s.generation(ctx, authority, repository, digest)
	if err != nil {
		return err
	}
	if record.Tombstone {
		return errors.New("artifact digest has an active deletion tombstone")
	}
	return nil
}

func generationKey(authority, repository, reference string) string {
	hash := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + reference))
	return "generations/" + hex.EncodeToString(hash[:]) + ".json"
}

func tagLeaseKey(authority, repository, tag string) string {
	return "tags/" + objectLeaseKey(authority, repository, tag)
}

func acquireContentLease(ctx context.Context, manager *lease.Manager, authority, repository, digest, owner string) (context.Context, func() error, error) {
	leaseCtx, _, release, err := acquireRenewedLease(ctx, manager, "objects/"+objectLeaseKey(authority, repository, digest), owner+"-"+strings.ReplaceAll(digest, ":", "-"), objectLeaseTTL)
	if err != nil {
		return nil, nil, err
	}
	return leaseCtx, release, nil
}

func acquireRenewedLease(ctx context.Context, manager *lease.Manager, key, owner string, ttl time.Duration) (context.Context, lease.Lease, func() error, error) {
	current, err := manager.Acquire(ctx, key, owner, ttl)
	if err != nil {
		return nil, lease.Lease{}, nil, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go renewLease(leaseCtx, cancel, manager, current, renewDone)
	return leaseCtx, current, func() error {
		cancel()
		renewErr := <-renewDone
		releaseErr := manager.Release(context.Background(), current)
		return errors.Join(renewErr, releaseErr)
	}, nil
}

func (s *Service) beginTagMutation(ctx context.Context, authority, repository, tag string) (context.Context, lease.Lease, generationRecord, func() error, error) {
	owner := requestID()
	leaseCtx, currentLease, release, err := acquireRenewedLease(ctx, s.tasks.Leases(), tagLeaseKey(authority, repository, tag), owner, 60*time.Second)
	if err != nil {
		return nil, lease.Lease{}, generationRecord{}, nil, err
	}
	record, err := s.advanceGeneration(leaseCtx, authority, repository, tag, false)
	if err != nil {
		_ = release()
		return nil, lease.Lease{}, generationRecord{}, nil, err
	}
	return leaseCtx, currentLease, record, release, nil
}
