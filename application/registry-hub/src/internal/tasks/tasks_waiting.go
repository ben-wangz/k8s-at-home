package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) WakeOldestWaitingQuota(ctx context.Context) (bool, error) {
	return m.WakeOldestWaitingQuotaFor(ctx, "distribution_quota")
}

func (m *Manager) WakeOldestWaitingQuotaFor(ctx context.Context, reason string) (bool, error) {
	oldest, found, err := m.OldestWaitingQuota(ctx, reason)
	if err != nil || !found {
		return false, err
	}
	if _, err := m.Wake(ctx, oldest.ID); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) OldestWaitingQuota(ctx context.Context, reason string) (Task, bool, error) {
	if !validWaitReason(reason) {
		return Task{}, false, errors.New("quota wait reason is invalid")
	}
	var oldest Task
	token := ""
	for {
		items, next, err := m.List(ctx, "", token, 1000)
		if err != nil {
			return Task{}, false, err
		}
		for _, item := range items {
			if !hasQuotaTurn(item, reason) {
				continue
			}
			if oldest.ID == "" || quotaWaitBefore(item, oldest) {
				oldest = item
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if oldest.ID == "" || oldest.Status != WaitingQuota {
		return Task{}, false, nil
	}
	return oldest, true, nil
}

func (m *Manager) IsQuotaTurn(ctx context.Context, taskID, reason string) (bool, error) {
	return m.IsQuotaTurnAt(ctx, taskID, reason, time.Time{})
}

func (m *Manager) IsQuotaTurnAt(ctx context.Context, taskID, reason string, queuedAt time.Time) (bool, error) {
	current, err := m.Get(ctx, taskID)
	if err != nil {
		return false, err
	}
	if !queuedAt.IsZero() {
		current.QuotaWaitAt = queuedAt
		current.QuotaWaitReason = reason
	}
	token := ""
	for {
		items, next, err := m.List(ctx, "", token, 1000)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if item.ID == current.ID || !hasQuotaTurn(item, reason) {
				continue
			}
			if quotaWaitBefore(item, current) {
				return false, nil
			}
		}
		if next == "" {
			return true, nil
		}
		token = next
	}
}

func hasQuotaTurn(task Task, reason string) bool {
	if task.CancelRequested {
		return false
	}
	if task.Status == WaitingQuota {
		return task.WaitReason == reason
	}
	return (task.Status == Pending || task.Status == Running) && task.QuotaWaitReason == reason && !task.QuotaWaitAt.IsZero()
}

func quotaWaitBefore(first, second Task) bool {
	firstAt := first.QuotaWaitAt
	if firstAt.IsZero() {
		firstAt = first.CreatedAt
	}
	secondAt := second.QuotaWaitAt
	if secondAt.IsZero() {
		secondAt = second.CreatedAt
	}
	if firstAt.Equal(secondAt) {
		return first.ID < second.ID
	}
	return firstAt.Before(secondAt)
}

func (m *Manager) ExpireIdle(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	cutoff := time.Now().UTC()
	var expired []string
	token := ""
	for len(expired) < limit {
		items, next, err := m.List(ctx, "", token, 1000)
		if err != nil {
			return len(expired), err
		}
		for _, item := range items {
			if len(expired) >= limit {
				break
			}
			if item.ExpiresAt.IsZero() || item.ExpiresAt.After(cutoff) || item.Status == Cancelled || item.Status == RepairPending || !isActive(item.Status) && !item.GCProtected {
				continue
			}
			expired = append(expired, item.ID)
		}
		if next == "" {
			break
		}
		token = next
	}
	removed := 0
	for _, id := range expired {
		cancelled, err := m.CancelIfExpired(ctx, id, cutoff)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
				continue
			}
			return removed, err
		}
		if cancelled {
			removed++
		}
	}
	return removed, nil
}

func (m *Manager) CancelIfExpired(ctx context.Context, id string, cutoff time.Time) (bool, error) {
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return false, err
		}
		if task.ExpiresAt.IsZero() || task.ExpiresAt.After(cutoff) || task.Status == Cancelled || task.Status == RepairPending || !isActive(task.Status) && !task.GCProtected || task.CancelRequested || task.CleanupStarted {
			return false, nil
		}
		if task.Status == Running && !task.RootPublished {
			task.CancelRequested = true
			task.CancelPhase = "stop_requested"
		} else {
			task.Status = Cancelled
			task.CancelRequested = true
			task.CancelPhase = "quiesced"
		}
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := m.Get(ctx, id)
			if readErr != nil {
				return false, errors.Join(err, readErr)
			}
			if latest.ExpiresAt.After(cutoff) || latest.ExpiresAt.IsZero() {
				return false, nil
			}
			if latest.CancelRequested && (latest.Status == Running && latest.CancelPhase == "stop_requested" || latest.Status == Cancelled && latest.CancelPhase == "quiesced") {
				task = latest
			} else {
				continue
			}
		} else if err != nil {
			return false, err
		}
		if task.Status == Running {
			if !task.RootPublished {
				m.mu.Lock()
				cancel := m.cancels[id]
				m.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		} else {
			m.finishInFlight(ctx, &task)
			m.notifyTerminal(ctx, &task)
		}
		return true, nil
	}
	return false, storage.ErrUnavailable
}
