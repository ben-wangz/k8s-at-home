package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type backgroundScanStatus struct {
	StartedAt      time.Time `json:"startedAt"`
	ObservedAt     time.Time `json:"observedAt"`
	ProgressAt     time.Time `json:"progressAt"`
	ErrorAt        time.Time `json:"errorAt,omitempty"`
	ErrorEntity    string    `json:"errorEntity,omitempty"`
	ErrorClass     string    `json:"errorClass,omitempty"`
	NextAttemptAt  time.Time `json:"nextAttemptAt,omitempty"`
	ErrorRecovered bool      `json:"errorRecovered"`
	Stale          bool      `json:"stale"`
	Complete       bool      `json:"complete"`
}

type entityBackoff struct {
	retries int
	next    time.Time
	round   uint64
}

func (s *Service) runSourceSweep(ctx context.Context, kind, prefix string, step func(context.Context, storage.Entry) error) {
	cursor := inventory.Cursor{}
	status := backgroundScanStatus{StartedAt: time.Now().UTC()}
	backoff := map[string]entityBackoff{}
	round, scanRetries := uint64(1), 0
	roundFailed := false
	nextRound, lastSave := time.Time{}, time.Time{}
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	prior, _, readErr := inventory.Read[backgroundScanStatus](initial, s.store, "background/v1/scans/"+kind+".json")
	initialCancel()
	if readErr == nil {
		status = prior
		status.StartedAt = time.Now().UTC()
		status.Complete = false
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(nextRound) {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		release, ok := s.backgroundBudget(bounded)
		if !ok {
			cancel()
			continue
		}
		entry, found, err := cursor.Peek(bounded, s.store, prefix, 32)
		if err == nil {
			scanRetries = 0
		}
		if err == nil && found {
			id := entry.Key + "\x00" + entry.Version
			if b, exists := backoff[id]; exists {
				b.round = round
				backoff[id] = b
			}
			for previous := range backoff {
				if strings.HasPrefix(previous, entry.Key+"\x00") && previous != id {
					delete(backoff, previous)
				}
			}
			if !backoff[id].next.After(time.Now()) {
				err = step(bounded, entry)
				if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
					status.Complete = false
					b := backoff[id]
					b.retries++
					b.round = round
					b.next = time.Now().Add(min(time.Second*time.Duration(1<<min(b.retries, 8)), 5*time.Minute))
					b.next = b.next.Add(time.Duration(time.Now().UnixNano() % int64(time.Second)))
					if len(backoff) < 1024 || b.retries > 1 {
						backoff[id] = b
					} else {
						nextRound = b.next
					}
					roundFailed = true
					status.ErrorAt, status.ErrorEntity, status.ErrorClass, status.NextAttemptAt, status.ErrorRecovered = time.Now().UTC(), entry.Key, "storage", b.next, false
					s.backgroundEvent(kind, "entity_failed", err)
				} else {
					delete(backoff, id)
					status.ProgressAt = time.Now().UTC()
					if status.ErrorEntity == entry.Key {
						status.ErrorRecovered = true
					}
				}
			}
			cursor.Advance()
		}
		if err != nil && !found {
			scanRetries++
			nextRound = time.Now().Add(min(time.Second*time.Duration(1<<min(scanRetries, 8)), 5*time.Minute))
			status.Complete, roundFailed = false, true
			status.ErrorAt, status.ErrorClass, status.ErrorEntity, status.NextAttemptAt, status.ErrorRecovered = time.Now().UTC(), "storage", prefix, nextRound, false
			s.backgroundEvent(kind, "scan_failed", err)
		}
		if cursor.Done {
			for id, b := range backoff {
				if b.round != round {
					delete(backoff, id)
				}
			}
			status.Complete, status.ObservedAt = len(backoff) == 0 && !roundFailed, time.Now().UTC()
			if status.Complete {
				status.ErrorRecovered = !status.ErrorAt.IsZero()
				status.NextAttemptAt = time.Time{}
			}
			round++
			roundFailed = false
			cursor = inventory.Cursor{}
			nextRound = time.Now().Add(15 * time.Second)
		}
		if time.Since(lastSave) > 5*time.Second {
			_ = inventory.Update(bounded, s.store, "background/v1/scans/"+kind+".json", func(value *backgroundScanStatus) error { *value = status; return nil })
			lastSave = time.Now()
		}
		release()
		cancel()
	}
}
