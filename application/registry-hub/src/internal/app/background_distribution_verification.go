package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"slices"
	"time"
)

type distributionVerificationWork struct {
	Authority      string `json:"authority"`
	Repository     string `json:"repository"`
	Digest         string `json:"digest"`
	MediaType      string `json:"mediaType"`
	Size           int64  `json:"size"`
	TaskID         string `json:"taskID"`
	AttemptID      string `json:"attemptID"`
	RoundID        string `json:"roundID"`
	TaskGeneration uint64 `json:"taskGeneration"`
	Generation     uint64 `json:"generation"`
	GCEpoch        uint64 `json:"gcEpoch"`
	SessionID      string `json:"sessionID"`
}

type distributionVerificationResult struct {
	FailureClass string `json:"failureClass,omitempty"`
}

func (r distributionVerificationResult) CompletionSummary(workqueue.Job) (json.RawMessage, error) {
	return json.Marshal(r)
}

func (s *solidifier) deferBlobVerification(ctx context.Context, task *tasks.Task, blob contentRef, sessionID string, generation, epoch uint64) error {
	work := distributionVerificationWork{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest,
		MediaType: blob.mediaType, Size: blob.size, TaskID: task.ID, AttemptID: task.AttemptID, RoundID: task.RoundID,
		TaskGeneration: task.Generation, Generation: generation, GCEpoch: epoch, SessionID: sessionID}
	source := workqueue.ID(work.Authority, work.Repository, work.Digest, fmt.Sprint(work.Size), work.TaskID, work.RoundID,
		task.AttemptID, work.SessionID, fmt.Sprint(generation), fmt.Sprint(epoch))
	job, err := s.service.backgroundQueue().Enqueue(ctx, "distribution_verification", source, work, time.Unix(1, 0).UTC())
	if err != nil {
		return temporary("verification_enqueue", err)
	}
	s.service.signalBackgroundQueue("distribution_verification")
	task.VerificationWorkID = job.ID
	return tasks.WaitFor(tasks.WaitingDeps, "distribution_verification")
}

func (s *Service) distributionVerificationSource(ctx context.Context, work distributionVerificationWork) (tasks.Task, bool, error) {
	task, err := s.tasks.Get(ctx, work.TaskID)
	if errors.Is(err, storage.ErrNotFound) {
		return task, false, nil
	}
	if err != nil {
		return task, false, err
	}
	valid := task.Authority == work.Authority && task.Repository == work.Repository && task.RoundID == work.RoundID &&
		task.Generation == work.TaskGeneration && task.AttemptID == work.AttemptID && !task.CancelRequested && !isTerminalTask(task.Status)
	if work.SessionID != "" && task.ProtectionSessionID != work.SessionID && !slices.Contains(task.SessionIDs, work.SessionID) {
		valid = false
	}
	return task, valid, nil
}

func (s *Service) distributionVerificationStep(ctx context.Context, job *workqueue.Job) (_ any, done bool, result error) {
	var work distributionVerificationWork
	if json.Unmarshal(job.Payload, &work) != nil || work.Authority == "" || work.Repository == "" ||
		artifactDigest(work.Digest) != nil || work.Size < 0 || work.TaskID == "" || work.GCEpoch == 0 {
		return nil, false, storage.ErrCorruptObject
	}
	task, valid, err := s.distributionVerificationSource(ctx, work)
	if err != nil || !valid {
		job.Superseded = !valid && err == nil
		return nil, err == nil, err
	}
	defer func() {
		if result == nil && done {
			result = s.wakeVerificationTask(ctx, task)
		}
	}()
	processor := &solidifier{service: s, cfg: s.cfg, registry: s.registry, tasks: s.tasks, gate: s.gate}
	current, err := s.generation(ctx, work.Authority, work.Repository, work.Digest)
	if err != nil {
		return nil, false, err
	}
	if current.Generation != work.Generation || !processor.observationEpochOpen(work.GCEpoch) {
		job.Superseded = true
		return nil, true, nil
	}
	finish, err := processor.beginProtectedOperation(ctx, work.SessionID)
	if err != nil {
		return nil, false, err
	}
	defer processor.finishProtectedOperation(finish, &result)
	leaseCtx, release, err := acquireContentLease(ctx, s.tasks.Leases(), work.Authority, work.Repository, work.Digest, "verification-"+job.ID)
	if errors.Is(err, lease.ErrHeld) {
		return nil, false, &workqueue.DependencyWait{Reason: "object_lease", RecheckAt: time.Now().UTC().Add(time.Second)}
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { result = errors.Join(result, release()) }()
	leaseCtx = inventory.WithWriteGuard(leaseCtx, func(check context.Context) error {
		_, valid, err := s.distributionVerificationSource(check, work)
		if err != nil {
			return err
		}
		if !valid {
			return storage.ErrConditionFailed
		}
		return nil
	})
	blob := contentRef{digest: work.Digest, mediaType: work.MediaType, size: work.Size}
	present, err := processor.headDistributionBlob(leaseCtx, &task, blob)
	if err == nil && present {
		path := "/v2/" + processor.internalName(work.Authority, work.Repository) + "/blobs/" + work.Digest
		err = processor.coordinateBlobVerification(leaseCtx, &task, blob, work.SessionID, path, work.Generation, work.GCEpoch)
	}
	if err != nil {
		var yielded *tasks.Yield
		if errors.As(err, &yielded) {
			return nil, false, &workqueue.DependencyWait{Reason: yielded.Reason, RecheckAt: time.Now().UTC().Add(time.Second)}
		}
		var failure *tasks.Failure
		if errors.As(err, &failure) && failure.Permanent && (failure.Class == "distribution_invalid" || failure.Class == "manifest_invalid") {
			if guardErr := inventory.ValidateWriteGuard(leaseCtx); guardErr != nil {
				return nil, false, guardErr
			}
			if stableErr := processor.requireStableObservationEpoch(leaseCtx, &task, work.Digest, work.Generation, work.GCEpoch); stableErr != nil {
				return nil, false, stableErr
			}
			return distributionVerificationResult{FailureClass: failure.Class}, true, nil
		}
		return nil, false, err
	}
	return nil, true, nil
}

func (s *solidifier) consumeVerificationResult(ctx context.Context, task *tasks.Task) error {
	if task.VerificationWorkID == "" {
		return nil
	}
	job, err := s.service.backgroundQueue().Read(ctx, "distribution_verification", task.VerificationWorkID)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrCorruptObject) {
		task.VerificationWorkID = ""
		return nil
	}
	if err != nil {
		return temporary("task_state", err)
	}
	if job.State != "done" && job.State != "failed" {
		return tasks.WaitFor(tasks.WaitingDeps, "distribution_verification")
	}
	task.VerificationWorkID = ""
	if job.State == "failed" || job.Superseded {
		return nil
	}
	var work distributionVerificationWork
	var result distributionVerificationResult
	if json.Unmarshal(job.Payload, &work) != nil || json.Unmarshal(job.FinalSummary, &result) != nil || result.FailureClass == "" {
		return nil
	}
	if work.TaskID != task.ID || work.RoundID != task.RoundID || work.TaskGeneration != task.Generation || work.Authority != task.Authority || work.Repository != task.Repository {
		return nil
	}
	if result.FailureClass != "distribution_invalid" && result.FailureClass != "manifest_invalid" {
		return nil
	}
	if err := s.requireStableObservationEpoch(ctx, task, work.Digest, work.Generation, work.GCEpoch); err != nil {
		return err
	}
	return permanent(result.FailureClass, errors.New("Distribution object failed verification"))
}

func (s *Service) wakeVerificationTask(ctx context.Context, expected tasks.Task) error {
	current, err := s.tasks.Get(ctx, expected.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.RoundID != expected.RoundID || current.Generation != expected.Generation || current.AttemptID != expected.AttemptID || current.CancelRequested {
		return nil
	}
	if current.Status != tasks.Running && current.Status != tasks.Pending && !(current.Status == tasks.WaitingDeps && current.WaitReason == "distribution_verification") {
		return nil
	}
	_, err = s.tasks.WakeMatching(ctx, current)
	if errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}

func (s *Service) reconcileVerificationWait(ctx context.Context, task tasks.Task) error {
	if task.VerificationWorkID != "" {
		job, err := s.backgroundQueue().Read(ctx, "distribution_verification", task.VerificationWorkID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
			return err
		}
		if err == nil && job.State != "done" && job.State != "failed" {
			return nil
		}
	}
	_, err := s.tasks.WakeMatching(ctx, task)
	if errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}
