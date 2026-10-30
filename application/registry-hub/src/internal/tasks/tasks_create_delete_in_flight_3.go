package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"registry-hub/internal/diagnostics"
	"time"
)

func (m *Manager) deleteInFlight(ctx context.Context, requestKey string) error {
	key := "tasks/inflight/" + requestKey
	entry, err := m.store.Head(ctx, key)
	if err != nil {
		return err
	}
	return m.store.DeleteIfMatch(ctx, key, entry.Version)
}

func (m *Manager) finishInFlight(ctx context.Context, task *Task) {
	requestKey := inFlightKey(task.Authority, task.Repository, task.Reference)
	key := "tasks/inflight/" + requestKey
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	if err != nil {
		return
	}
	var index struct {
		TaskID string `json:"taskID"`
	}
	if json.Unmarshal(data, &index) != nil || index.TaskID != task.ID {
		return
	}
	_ = m.store.DeleteIfMatch(ctx, key, object.Version)
}

func taskKey(id string) string { return "tasks/state/" + id + ".json" }

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func inFlightKey(authority, repository, reference string) string {
	hash := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + reference))
	return hex.EncodeToString(hash[:])
}

func retryDelay(retry int) time.Duration {
	if retry > 9 {
		retry = 9
	}
	if retry < 1 {
		retry = 1
	}
	delay := time.Second << (retry - 1)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func safeMessage(cause error) string { return diagnostics.SafeErrorText(cause) }
