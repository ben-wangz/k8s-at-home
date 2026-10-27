package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) finalizeReusedAnchoredManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest) (bool, error) {
	release, err := s.gate.Enter(ctx)
	if err != nil {
		if errors.Is(err, ErrWritesClosed) || errors.Is(err, ErrRecoveryBarrier) {
			return false, tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
		}
		return false, temporary("protection_state", err)
	}
	defer release()
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return false, err
	}
	epoch := s.gate.State().Epoch
	if !s.observationEpochOpen(epoch) {
		return false, tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
	}
	scopeID, err := protectionScopeID(task.Authority, task.Repository, manifest.ref.digest, task.Generation)
	if err != nil {
		return false, permanent("task_state", err)
	}
	sessionID := stagedSessionID(scopeID)
	if manifest.scopeID != scopeID || manifest.sessionID != sessionID {
		return false, permanent("task_state", errors.New("reused manifest scope identity does not match the root task"))
	}
	session, err := s.gate.readProtectionSession(ctx, sessionID)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, temporary("protection_state", err)
	}
	terminalAnchored := session.State == "anchored" || session.State == "released" && session.TerminalReason == "anchored"
	if session.ID != sessionID || session.ScopeID != scopeID || !terminalAnchored || session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil {
		return false, temporary("protection_state", errors.New("reused manifest protection scope is not safely anchored"))
	}
	_, verifiedManifest, err := s.readDistributionManifest(ctx, task, manifest.ref)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	manifestGeneration, err := s.contentGeneration(ctx, task.Authority, task.Repository, manifest.ref.digest)
	if err != nil {
		return false, temporary("generation", err)
	}
	blobGenerations := make(map[string]uint64, len(manifest.blobs))
	for _, blob := range manifest.blobs {
		generation, err := s.contentGeneration(ctx, task.Authority, task.Repository, blob.digest)
		if err != nil {
			return false, temporary("generation", err)
		}
		present, err := s.verifyDistributionBlobAtEpoch(ctx, task, blob, generation, epoch)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
		blobGenerations[blob.digest] = generation
	}
	if err := s.recordManifestObservationAtGenerationEpoch(ctx, task, verifiedManifest, manifestGeneration, epoch); err != nil {
		return false, err
	}
	if err := s.requireStableObservationEpoch(ctx, task, manifest.ref.digest, manifestGeneration, epoch); err != nil {
		return false, err
	}
	for _, blob := range manifest.blobs {
		if err := s.requireStableObservationEpoch(ctx, task, blob.digest, blobGenerations[blob.digest], epoch); err != nil {
			return false, err
		}
	}
	if err := s.saveManifestGraphNode(ctx, task, verifiedManifest, true, "anchored"); err != nil {
		return false, err
	}
	if err := s.finalizeAnchoredManifestBlobs(ctx, task, manifest); err != nil {
		return false, err
	}
	if err := s.requireStableObservationEpoch(ctx, task, manifest.ref.digest, manifestGeneration, epoch); err != nil {
		return false, err
	}
	for _, blob := range manifest.blobs {
		if err := s.requireStableObservationEpoch(ctx, task, blob.digest, blobGenerations[blob.digest], epoch); err != nil {
			return false, err
		}
		if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, blob.digest); err != nil {
			return false, temporary("storage", err)
		}
	}
	if err := s.requireStableObservationEpoch(ctx, task, manifest.ref.digest, manifestGeneration, epoch); err != nil {
		return false, err
	}
	if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, manifest.ref.digest); err != nil {
		return false, temporary("storage", err)
	}
	if err := s.requireStableObservationEpoch(ctx, task, manifest.ref.digest, manifestGeneration, epoch); err != nil {
		return false, err
	}
	return true, nil
}

func (s *solidifier) requireStableObservationEpoch(ctx context.Context, task *tasks.Task, digest string, generation, epoch uint64) error {
	if !s.observationEpochOpen(epoch) {
		return tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
	}
	current, err := s.contentGeneration(ctx, task.Authority, task.Repository, digest)
	if err != nil {
		return temporary("generation", err)
	}
	if current != generation {
		return temporary("generation", errors.New("content generation changed during Distribution verification"))
	}
	if !s.observationEpochOpen(epoch) {
		return tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
	}
	return nil
}

func (s *solidifier) observationEpochOpen(epoch uint64) bool {
	state := s.gate.State()
	return state.State == "open" && !state.Exclusive && !state.RecoveryBarrier && state.Epoch == epoch
}

func (s *solidifier) verifyDistributionBlobAtEpoch(ctx context.Context, task *tasks.Task, blob contentRef, generation, epoch uint64) (bool, error) {
	present, err := s.headDistributionBlob(ctx, task, blob)
	if err != nil || !present {
		return present, err
	}
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/blobs/" + blob.digest
	if err := s.verifyOrReuseLocalBlobAtGenerationEpoch(ctx, task, blob, "", path, generation, epoch); err != nil {
		return false, err
	}
	return true, nil
}

func (s *solidifier) headDistributionBlob(ctx context.Context, task *tasks.Task, blob contentRef) (bool, error) {
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/blobs/" + blob.digest
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return false, temporary("distribution", err)
	}
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodHead, route.EndpointBlob)
		_ = response.Body.Close()
		if miss {
			return false, nil
		}
		return false, temporary("distribution", errors.New("Distribution returned an untrusted blob miss"))
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, temporary("distribution", fmt.Errorf("Distribution blob HEAD returned status %d", response.StatusCode))
	}
	if response.ContentLength != blob.size || response.Header.Get("Docker-Content-Digest") != blob.digest {
		return false, permanent("distribution_invalid", errors.New("existing Distribution blob does not match its descriptor"))
	}
	return true, nil
}

func (s *solidifier) recordManifestObservationAtGenerationEpoch(ctx context.Context, task *tasks.Task, manifest contentRef, generation, epoch uint64) error {
	if err := s.requireStableObservationEpoch(ctx, task, manifest.digest, generation, epoch); err != nil {
		return err
	}
	_, err := s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphManifest, manifest.digest, manifest.size, generation, epoch)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	operationID := fmt.Sprintf("%s-%s-%d-%d", task.ID, objectLeaseKey(task.Authority, task.Repository, manifest.digest), generation, epoch)
	if err := s.service.records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphManifest,
		Digest: manifest.digest, ActualSize: manifest.size, Generation: generation,
		GCEpoch: epoch, OperationID: operationID, VerificationBasis: "distribution_readback",
	}); err != nil {
		return temporary("task_state", err)
	}
	return s.requireStableObservationEpoch(ctx, task, manifest.digest, generation, epoch)
}
