package app

import (
	"context"
	"encoding/json"
	"errors"
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

func (s *Service) deferDownloadCleanup(ctx context.Context, authority, repository, identity, digest string, fence uint64) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticket, err := s.downloadManager().CompletedTicket(cleanupCtx, identity, digest, fence)
	if err != nil {
		s.backgroundEvent("download_cleanup", "enqueue_failed", err)
		return err
	}
	cached, err := s.cache.GetAnyRetained(cleanupCtx, authority, repository, digest)
	if err != nil {
		s.backgroundEvent("download_cleanup", "enqueue_failed", err)
		return err
	}
	_ = cached.Body.Close()
	_, err = (workqueue.Queue{Store: s.store}).Enqueue(cleanupCtx, "download_cleanup", ticket.Identity+"/"+ticket.Session+"/"+ticket.StateVersion+"/"+cached.DataVersion, downloadCleanupWork{CleanupTicket: ticket, Authority: authority, Repository: repository, CacheVersion: cached.DataVersion}, ticket.CompletedAt)
	if err != nil {
		s.backgroundEvent("download_cleanup", "enqueue_failed", err)
	}
	return err
}

func (s *Service) cleanupDownloadStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var source downloadCleanupWork
	var progress download.CleanupProgress
	if json.Unmarshal(job.Payload, &source) != nil || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &progress) != nil {
		return progress, false, storage.ErrCorruptObject
	}
	leaseCtx, objectLease, release, err := acquireRenewedLease(ctx, s.tasks.Leases(), "objects/"+source.Identity, "committed-cleanup-"+requestID(), objectLeaseTTL)
	if errors.Is(err, lease.ErrHeld) {
		return progress, false, nil
	}
	if err != nil {
		return progress, false, err
	}
	defer release()
	present, err := s.downloadManager().CleanupTicketCurrent(leaseCtx, source.CleanupTicket)
	if err != nil {
		return progress, false, err
	}
	if progress.Phase < 2 && present {
		cached, err := s.cache.GetAnyRetained(leaseCtx, source.Authority, source.Repository, source.Digest)
		if err != nil {
			return progress, false, err
		}
		_ = cached.Body.Close()
		if cached.DataVersion != source.CacheVersion {
			return progress, true, nil
		}
	}
	guard := func(ctx context.Context) error {
		if err := (workqueue.Queue{Store: s.store}).Validate(ctx, *job); err != nil {
			return err
		}
		return s.tasks.Leases().Validate(ctx, objectLease)
	}
	done, err := s.downloadManager().CleanupCommittedStep(leaseCtx, source.CleanupTicket, &progress, guard)
	return progress, done, err
}
