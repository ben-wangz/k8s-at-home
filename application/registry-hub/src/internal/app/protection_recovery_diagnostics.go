package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type protectionRecoveryProgress struct {
	Generation            string    `json:"generation"`
	Owner                 string    `json:"owner"`
	Fence                 uint64    `json:"fence"`
	GateEpoch             uint64    `json:"gateEpoch"`
	Phase                 string    `json:"phase"`
	GateState             string    `json:"gateState"`
	Substep               string    `json:"substep,omitempty"`
	EntityID              string    `json:"entityID,omitempty"`
	Processed             uint64    `json:"processed"`
	CursorEntities        uint64    `json:"cursorEntities"`
	NestedEntities        uint64    `json:"nestedEntities"`
	PageOffset            int       `json:"pageOffset"`
	SessionOffset         int       `json:"sessionOffset"`
	EntityOffset          int       `json:"entityOffset"`
	RepresentationEntries uint64    `json:"representationEntries"`
	ReferenceHalf         int       `json:"referenceHalf"`
	RemoteVerifiedBytes   int64     `json:"remoteVerifiedBytes"`
	Yields                uint64    `json:"yields"`
	Reason                string    `json:"reason,omitempty"`
	StartedAt             time.Time `json:"startedAt"`
	ProgressAt            time.Time `json:"progressAt"`
	CheckpointAt          time.Time `json:"checkpointAt"`
	NextAttemptAt         time.Time `json:"nextAttemptAt"`
}

func (s *Service) observeRecoveryCheckpoint(c protectionRecoveryCheckpoint) {
	progress := &protectionRecoveryProgress{Generation: c.Generation, Owner: c.Owner, Fence: c.Fence, GateEpoch: c.Epoch, Phase: c.Phase, Substep: c.Step, EntityID: c.Entity, Processed: c.Processed, CursorEntities: c.Cursor.Entities, NestedEntities: c.Inner.Entities, PageOffset: c.Cursor.Offset, SessionOffset: c.SessionIndex, ReferenceHalf: c.Half, RemoteVerifiedBytes: c.Remote.Offset, Yields: c.Yields, Reason: c.Reason, StartedAt: c.StartedAt, ProgressAt: c.ProgressAt, CheckpointAt: c.UpdatedAt, NextAttemptAt: c.NextAttemptAt}
	if s.gate != nil {
		progress.GateState = s.gate.State().State
	}
	progress.EntityOffset, progress.RepresentationEntries = c.Index, c.Representations.Cursor.Entities
	if progress.EntityID == "" && (c.Phase == "remote_operations" || c.Phase == "validate_initial" || c.Phase == "validate_final") && c.Index < len(c.SessionIDs) {
		progress.EntityID = c.SessionIDs[c.Index]
	}
	s.updateWorker("protection_recovery", func(state *workerStatus) {
		if state.Recovery != nil && state.Recovery.CheckpointAt.After(c.UpdatedAt) {
			return
		}
		state.Recovery, state.Phase, state.Resumability = progress, c.Phase, "durable_phase_and_substep"
		state.LastEntityProgressAt = c.ProgressAt
		if c.LastError != nil && (state.LastError == nil || !state.LastError.OccurredAt.After(c.LastError.OccurredAt)) {
			state.LastError, state.RecoveredAt = c.LastError, c.RecoveredAt
		}
	})
	s.updateWorker("index_repair", func(state *workerStatus) {
		state.Resumability, state.Phase, state.State = "durable_phase_and_substep", "representation_indexes", "waiting_for_protection_recovery"
		if c.Phase == "representation_indexes" {
			state.State = "execution"
		} else if c.Representations.Done {
			state.State = "completed"
		}
		state.LastEntityProgressAt = c.ProgressAt
	})
}

func (s *Service) readRecoveryDiagnostics(ctx context.Context) string {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, err := inventory.Read[protectionRecoveryCheckpoint](bounded, s.store, protectionRecoveryKey)
	if errors.Is(err, storage.ErrNotFound) {
		return ""
	}
	if err != nil {
		return "recovery_checkpoint_" + diagnosticReason(err)
	}
	if c.Schema != 1 {
		return "recovery_checkpoint_corrupt"
	}
	s.observeRecoveryCheckpoint(c)
	return ""
}
