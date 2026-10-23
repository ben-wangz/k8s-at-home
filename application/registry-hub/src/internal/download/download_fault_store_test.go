package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"registry-hub/internal/storage"
)

type downloadFaultStore struct {
	storage.Store
	mu               sync.Mutex
	putCounts        map[string]int
	failStateOffset  int64
	failStateMode    string
	stateFailureUsed bool
	quotaFailureMode string
	quotaFailureUsed bool
	stateDelay       time.Duration
	slowStateOffset  int64
	quotaConflicts   int
}

func (s *downloadFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	s.mu.Lock()
	if s.putCounts == nil {
		s.putCounts = make(map[string]int)
	}
	s.putCounts[key]++
	if key == quotaKey && s.quotaConflicts > 0 {
		s.quotaConflicts--
		s.mu.Unlock()
		return storage.Entry{}, storage.ErrConditionFailed
	}
	quotaMode := ""
	if key == quotaKey && s.quotaFailureMode != "" && !s.quotaFailureUsed {
		s.quotaFailureUsed = true
		quotaMode = s.quotaFailureMode
	}
	stateOffset := int64(-1)
	if strings.HasSuffix(key, "/state.json") {
		var value struct {
			Offset int64 `json:"offset"`
		}
		if json.Unmarshal(data, &value) == nil {
			stateOffset = value.Offset
		}
	}
	mode := ""
	if stateOffset == s.failStateOffset && s.failStateMode != "" && !s.stateFailureUsed {
		s.stateFailureUsed = true
		mode = s.failStateMode
	}
	delay := time.Duration(0)
	if stateOffset == s.slowStateOffset {
		delay = s.stateDelay
	}
	s.mu.Unlock()
	if quotaMode == "not-applied" {
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}

	if mode == "error" || mode == "not-applied" {
		commitErr := storage.ErrTimeout
		if mode == "not-applied" {
			commitErr = storage.ErrUnknownCommit
		}
		return storage.Entry{}, errors.Join(commitErr, context.DeadlineExceeded)
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return storage.Entry{}, ctx.Err()
		case <-timer.C:
		}
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if quotaMode == "applied" && err == nil {
		return entry, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}
	if mode == "applied" && err == nil {
		return entry, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}
	return entry, err
}

func (s *downloadFaultStore) putCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCounts[key]
}
