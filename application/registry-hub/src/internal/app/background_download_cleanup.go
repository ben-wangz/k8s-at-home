package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

type downloadCleanupWork struct {
	download.CleanupTicket
	Authority    string `json:"authority"`
	Repository   string `json:"repository"`
	CacheVersion string `json:"cacheVersion"`
}

func (s *Service) deferDownloadCleanup(ctx context.Context, authority, repository, identity, digest string, fence uint64, committedVersion ...string) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticket, err := s.downloadManager().CompletedTicket(cleanupCtx, identity, digest, fence)
	if err != nil {
		s.backgroundEvent("download_cleanup", "enqueue_failed", err)
		return err
	}
	version := ""
	if len(committedVersion) > 0 {
		version = committedVersion[0]
	} else {
		cached, inspectErr := s.cache.InspectAny(cleanupCtx, authority, repository, digest, "application/octet-stream")
		if inspectErr != nil {
			return inspectErr
		}
		if !cached.Present {
			return storage.ErrUnavailable
		}
		version = cached.DataVersion
	}
	if version == "" {
		return storage.ErrCorruptObject
	}
	_, err = s.backgroundQueue().Enqueue(cleanupCtx, "download_cleanup", ticket.Identity+"/"+ticket.Session+"/"+ticket.StateVersion+"/"+version, downloadCleanupWork{CleanupTicket: ticket, Authority: authority, Repository: repository, CacheVersion: version}, ticket.CompletedAt)
	if err != nil {
		s.backgroundEvent("download_cleanup", "enqueue_failed", err)
	}
	return err
}

func (s *Service) cleanupDownloadStep(ctx context.Context, job *workqueue.Job) (result any, done bool, cause error) {
	var source downloadCleanupWork
	var progress download.CleanupProgress
	if json.Unmarshal(job.Payload, &source) != nil || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &progress) != nil {
		return progress, false, storage.ErrCorruptObject
	}
	leaseCtx, objectLease, release, err := s.acquireBackgroundUnitLease(ctx, "download_cleanup", s.tasks.Leases(), "objects/"+source.Identity, "committed-cleanup-"+requestID(), objectLeaseTTL)
	if errors.Is(err, lease.ErrHeld) {
		job.WaitReason = "object_lease_held"
		return progress, false, nil
	}
	if err != nil {
		return progress, false, err
	}
	defer release()
	generation, err := s.observeGeneration(leaseCtx, source.Authority, source.Repository, source.Digest)
	if err != nil {
		return progress, false, err
	}
	present, err := s.downloadManager().CleanupTicketCurrent(leaseCtx, source.CleanupTicket)
	if err != nil {
		return progress, false, err
	}
	var proof *artifact.VerifiedPin
	if progress.Phase < 2 && present {
		proof, err = s.cache.VerifyAnyPinned(storage.WithIOPurpose(leaseCtx, "cleanup_verification"), source.Authority, source.Repository, source.Digest)
		if err != nil {
			return progress, false, err
		}
		defer func() { cause = errors.Join(cause, proof.Close()) }()
		if proof.DataVersion != source.CacheVersion {
			job.Superseded = true
			return progress, true, nil
		}
		leaseCtx = storage.WithIOPurpose(proof.Context(), storage.IOPurpose(leaseCtx))
	}
	guard := func(ctx context.Context) error {
		if s.recoveryBlocked() {
			return ErrRecoveryBarrier
		}
		if err := (workqueue.Queue{Store: s.store}).Validate(ctx, *job); err != nil {
			return err
		}
		return s.tasks.Leases().Validate(ctx, objectLease)
	}
	return s.cleanupDownloadBatches(leaseCtx, job, source, progress, proof, generation, guard)
}
