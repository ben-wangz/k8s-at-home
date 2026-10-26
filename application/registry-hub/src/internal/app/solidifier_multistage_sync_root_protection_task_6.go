package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) syncRootProtectionTask(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string, requestedBytes int64) error {
	latest, err := s.tasks.Get(ctx, task.ID)
	if err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_read")
	}
	if latest.CancelRequested || latest.FencingToken != taskLease.FencingToken {
		return tasks.ErrCancelled
	}
	contains := false
	for _, id := range latest.SessionIDs {
		if id == sessionID {
			contains = true
			break
		}
	}
	if !contains {
		latest.SessionIDs = append(latest.SessionIDs, sessionID)
	}
	var reserved int64
	seen := make(map[string]bool, len(latest.SessionIDs))
	for _, id := range latest.SessionIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		session, active, err := s.gate.protectionSession(ctx, id)
		if err != nil {
			return withSolidificationFailureStage(temporary("protection_state", err), "protection_reconcile")
		}
		if active {
			sessionBytes, err := taskProtectionReservationBytes(&latest, id, session)
			if err != nil {
				return withSolidificationFailureStage(temporary("protection_state", err), "protection_reconcile")
			}
			if sessionBytes > math.MaxInt64-reserved {
				return temporary("protection_state", errors.New("task protection reservation overflows"))
			}
			reserved += sessionBytes
		}
	}
	latest.ScopeID = task.ScopeID
	latest.RequestedBytes = requestedBytes
	latest.ReservedBytes = reserved
	latest.GCProtected = len(latest.SessionIDs) > 0
	if err := s.tasks.SaveProgress(ctx, &latest, taskLease); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	*task = latest
	return nil
}

func (s *solidifier) recordStagedManifestObservation(ctx context.Context, task *tasks.Task, manifest contentRef) error {
	operationID := fmt.Sprintf("%s-%s-%d", task.ID, objectLeaseKey(task.Authority, task.Repository, manifest.digest), s.gate.State().Epoch)
	if err := s.service.records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphManifest,
		Digest: manifest.digest, ActualSize: manifest.size, Generation: task.Generation,
		GCEpoch: s.gate.State().Epoch, OperationID: operationID, VerificationBasis: "distribution_readback",
	}); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	return nil
}

func (s *solidifier) markStagedManifestPublished(ctx context.Context, task *tasks.Task, manifest *stagedManifest) error {
	if err := s.saveManifestGraphNode(ctx, task, manifest.ref, true, "published_unanchored"); err != nil {
		return err
	}
	return nil
}

func (s *solidifier) prepareStagedBlob(ctx context.Context, task *tasks.Task, manifest *stagedManifest, blob contentRef) (bool, tasks.BlobIntent, error) {
	cached, err := s.cache.GetAnyRetained(ctx, task.Authority, task.Repository, blob.digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, tasks.BlobIntent{}, nil
	}
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("storage", err)
	}
	defer cached.Body.Close()
	if cached.Size != blob.size {
		return false, tasks.BlobIntent{}, permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
	}
	generation, err := s.blobContentGeneration(ctx, task, blob.digest)
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("generation", err)
	}
	intent, _, err := s.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest}, generation, "")
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	demandID := stagedDemandID(task.ID, manifest.scopeID)
	if err := s.service.records.LinkBlobDemandScope(ctx, intent.ID, demandID, task.ID, manifest.scopeID, manifest.sessionID, manifest.scopeBytes); err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	ready, err := s.service.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	if ready.ID != intent.ID {
		if err := s.service.records.LinkBlobDemandScope(ctx, ready.ID, demandID, task.ID, manifest.scopeID, manifest.sessionID, manifest.scopeBytes); err != nil {
			return false, tasks.BlobIntent{}, temporary("task_state", err)
		}
	}
	return true, ready, nil
}

func (s *solidifier) anchorManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest) error {
	session, active, err := s.gate.protectionSession(ctx, manifest.sessionID)
	if err != nil {
		return temporary("protection_state", err)
	}
	if active {
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return tasks.WaitFor(tasks.WaitingDeps, "remote_operations_pending")
		}
	}
	if err := s.saveManifestGraphNode(ctx, task, manifest.ref, true, "anchored"); err != nil {
		return err
	}
	intentIDs, err := s.markManifestBlobIntentsAnchored(ctx, task, manifest)
	if err != nil {
		return err
	}
	if active {
		if err := s.gate.MarkProtectionAnchored(ctx, manifest.sessionID); err != nil {
			return temporary("protection_anchor", err)
		}
	}
	if err := s.removeRootProtectionSession(ctx, task, taskLease, manifest.sessionID); err != nil {
		return err
	}
	if active {
		if err := s.gate.ReleaseProtection(ctx, manifest.sessionID, "anchored"); err != nil {
			return temporary("protection_release", err)
		}
	}
	return s.anchorBlobTasks(ctx, intentIDs)
}

func (s *solidifier) finalizeAnchoredManifestBlobs(ctx context.Context, task *tasks.Task, manifest *stagedManifest) error {
	intentIDs, err := s.markManifestBlobIntentsAnchored(ctx, task, manifest)
	if err != nil {
		return err
	}
	return s.anchorBlobTasks(ctx, intentIDs)
}

func (s *solidifier) markManifestBlobIntentsAnchored(ctx context.Context, task *tasks.Task, manifest *stagedManifest) ([]string, error) {
	intentIDs := make([]string, 0, len(manifest.blobs))
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, temporary("task_state", err)
		}
		if _, err := s.service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentAnchored, "", ""); err != nil {
			return nil, temporary("task_state", err)
		}
		s.service.wakeBlobDemandRoots(ctx, intent.ID)
		intentIDs = append(intentIDs, intent.ID)
	}
	return intentIDs, nil
}

func (s *solidifier) anchorBlobTasks(ctx context.Context, intentIDs []string) error {
	for _, intentID := range intentIDs {
		if err := s.tasks.AnchorBlobTask(ctx, intentID); err != nil && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, tasks.ErrCancelled) {
			return temporary("task_state", err)
		}
	}
	return nil
}
