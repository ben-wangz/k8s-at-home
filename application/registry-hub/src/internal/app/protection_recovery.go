package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

const protectionRecoveryKey = "system/recovery/protection.json"
const protectionRecoveryCheckpointBudget = 15 * time.Second

var errProtectionRecoveryYield = errors.New("protection recovery yielded at a durable checkpoint")

type protectionRecoveryCheckpoint struct {
	Schema            int                             `json:"schema"`
	Generation        string                          `json:"generation"`
	Trigger           *recoveryTrigger                `json:"trigger,omitempty"`
	Owner             string                          `json:"owner"`
	Fence             uint64                          `json:"fence"`
	Epoch             uint64                          `json:"epoch"`
	Phase             string                          `json:"phase"`
	Step              string                          `json:"step,omitempty"`
	Cursor            inventory.Cursor                `json:"cursor"`
	Inner             inventory.Cursor                `json:"inner"`
	Entity            string                          `json:"entity,omitempty"`
	Index             int                             `json:"index,omitempty"`
	Half              int                             `json:"half,omitempty"`
	Digest            string                          `json:"digest,omitempty"`
	SessionIDs        []string                        `json:"sessionIDs,omitempty"`
	Owned             []string                        `json:"owned,omitempty"`
	SessionIndex      int                             `json:"sessionIndex,omitempty"`
	ReservedBytes     int64                           `json:"reservedBytes,omitempty"`
	ScopeID           string                          `json:"scopeID,omitempty"`
	ActiveIDs         []string                        `json:"activeIDs,omitempty"`
	Demand            *tasks.BlobDemand               `json:"demand,omitempty"`
	ActiveDemand      bool                            `json:"activeDemand,omitempty"`
	DemandCursor      inventory.Cursor                `json:"demandCursor"`
	LegacyOwners      map[string]bool                 `json:"legacyOwners,omitempty"`
	LegacyRequired    map[string]bool                 `json:"legacyRequired,omitempty"`
	LegacySeen        int                             `json:"legacySeen,omitempty"`
	LegacyBytes       int64                           `json:"legacyBytes,omitempty"`
	CacheReservations map[string]int64                `json:"cacheReservations,omitempty"`
	ValidationLedger  string                          `json:"validationLedger,omitempty"`
	NewTotal          int64                           `json:"newTotal,omitempty"`
	LegacyTotal       int64                           `json:"legacyTotal,omitempty"`
	NewCount          int                             `json:"newCount,omitempty"`
	Representations   artifact.RepresentationRecovery `json:"representations"`
	Remote            protectionRemoteCheckpoint      `json:"remote"`
	Processed         uint64                          `json:"processed"`
	Yields            uint64                          `json:"yields"`
	StartedAt         time.Time                       `json:"startedAt"`
	ProgressAt        time.Time                       `json:"progressAt"`
	UpdatedAt         time.Time                       `json:"updatedAt"`
	NextAttemptAt     time.Time                       `json:"nextAttemptAt"`
	Attempts          int                             `json:"attempts"`
	Reason            string                          `json:"reason,omitempty"`
	LastError         *diagnostics.Failure            `json:"lastError,omitempty"`
	RecoveredAt       time.Time                       `json:"recoveredAt"`
}

type protectionRecovery struct {
	service      *Service
	checkpoint   protectionRecoveryCheckpoint
	version      string
	manager      *lease.Manager
	lease        lease.Lease
	indexTask    *tasks.Task
	indexDigests []string
}

func (r *protectionRecovery) guard(ctx context.Context) error {
	if err := r.manager.Validate(ctx, r.lease); err != nil {
		if errors.Is(err, lease.ErrLost) {
			return errors.Join(storage.ErrLeaseLost, err)
		}
		return err
	}
	state, err := readRecoveryGate(ctx, r.service.store)
	if err != nil {
		return err
	}
	if !state.RecoveryBarrier && (r.checkpoint.Phase == "finalize" || r.checkpoint.Phase == "complete") && state.RecoveryID == r.checkpoint.Generation && state.Epoch == r.checkpoint.Epoch && recoveryLedger(state) == r.checkpoint.ValidationLedger {
		return nil
	}
	if state.RecoveryID != r.checkpoint.Generation || state.RecoveryFence != r.lease.FencingToken || state.Epoch != r.checkpoint.Epoch {
		return storage.ErrLeaseLost
	}
	if !state.RecoveryBarrier && r.checkpoint.Phase != "complete" && r.checkpoint.Phase != "finalize" {
		return storage.ErrLeaseLost
	}
	return nil
}

func (r *protectionRecovery) save(ctx context.Context) error {
	if parent, ok := ctx.Value(backgroundParentKey{}).(context.Context); ok {
		ctx = parent
	}
	bounded, cancel := context.WithTimeout(ctx, protectionRecoveryCheckpointBudget)
	defer cancel()
	if err := r.guard(bounded); err != nil {
		return err
	}
	r.checkpoint.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(r.checkpoint)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return storage.ErrCorruptObject
	}
	options := storage.PutOptions{IfAbsent: r.version == "", IfMatch: r.version}
	entry, err := r.service.store.Put(bounded, protectionRecoveryKey, bytes.NewReader(data), int64(len(data)), options)
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, version, readErr := inventory.Read[protectionRecoveryCheckpoint](bounded, r.service.store, protectionRecoveryKey)
		actual, _ := json.Marshal(current)
		if readErr == nil && bytes.Equal(actual, data) {
			entry.Version, err = version, nil
		}
	}
	if err == nil {
		r.version = entry.Version
		r.service.observeRecoveryCheckpoint(r.checkpoint)
	}
	return err
}
