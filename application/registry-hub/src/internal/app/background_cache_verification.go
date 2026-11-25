package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type cacheVerificationWork struct {
	Identity     tasks.BlobIdentity `json:"identity"`
	MediaType    string             `json:"mediaType"`
	Size         int64              `json:"size"`
	CacheVersion string             `json:"cacheVersion"`
	Fingerprint  string             `json:"fingerprint,omitempty"`
	Generation   uint64             `json:"generation"`
}

func (s *Service) inspectReadyBlob(ctx context.Context, cache *artifact.Cache, authority, repository, digest string) (artifact.Inspection, error) {
	inspection, err := cache.InspectAny(ctx, authority, repository, digest, "application/octet-stream")
	if err != nil || !inspection.Unknown {
		return inspection, err
	}
	generation, err := s.generation(ctx, authority, repository, digest)
	if err != nil {
		return inspection, err
	}
	work := cacheVerificationWork{Identity: tasks.BlobIdentity{Authority: authority, Repository: repository, Digest: digest}, MediaType: inspection.MediaType, Size: inspection.Size, CacheVersion: inspection.DataVersion, Fingerprint: inspection.PayloadFingerprint, Generation: generation.Generation}
	source := workqueue.ID(authority, repository, digest, work.MediaType, work.CacheVersion, work.Fingerprint, fmt.Sprint(work.Generation))
	observed := inspection.VerifiedAt
	if observed.IsZero() {
		observed = time.Unix(1, 0).UTC()
	}
	_, err = s.backgroundQueue().Enqueue(ctx, "cache_verification", source, work, observed)
	return inspection, err
}

func (s *Service) cacheVerificationStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var work cacheVerificationWork
	if json.Unmarshal(job.Payload, &work) != nil || work.Identity.Authority == "" || work.Identity.Repository == "" || work.Identity.Digest == "" || work.CacheVersion == "" || work.MediaType == "" || work.Size < 0 {
		return nil, false, storage.ErrCorruptObject
	}
	generation, err := s.generation(ctx, work.Identity.Authority, work.Identity.Repository, work.Identity.Digest)
	if err != nil {
		return nil, false, err
	}
	if generation.Generation != work.Generation {
		job.Superseded = true
		return nil, true, nil
	}
	if s.recoveryBlocked() {
		return nil, false, ErrRecoveryBarrier
	}
	inspection, err := s.cache.Inspect(ctx, work.Identity.Authority, work.Identity.Repository, work.Identity.Digest, work.MediaType)
	if err != nil {
		return nil, false, err
	}
	if inspection.DataVersion != work.CacheVersion || inspection.Size != work.Size || inspection.PayloadFingerprint != work.Fingerprint || !inspection.Present && !inspection.Unknown {
		job.Superseded = true
		return nil, true, nil
	}
	if !inspection.Present {
		expected := artifact.CachedRepresentation{Authority: work.Identity.Authority, Repository: work.Identity.Repository, Digest: work.Identity.Digest, MediaType: work.MediaType, Size: work.Size, DataVersion: work.CacheVersion}
		err = s.cache.RefreshVerification(ctx, expected, func(ctx context.Context) error {
			if s.recoveryBlocked() {
				return ErrRecoveryBarrier
			}
			current, err := s.generation(ctx, work.Identity.Authority, work.Identity.Repository, work.Identity.Digest)
			if err != nil {
				return err
			}
			if current.Generation != work.Generation {
				return storage.ErrConditionFailed
			}
			return s.backgroundQueue().Validate(ctx, *job)
		})
		if errors.Is(err, storage.ErrNotFound) {
			job.Superseded = true
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if s.metrics != nil {
			s.metrics.recordEvent("integrity_legacy_upgrade")
		}
	}
	s.scheduleCachedBlobIntent(work.Identity)
	return nil, true, nil
}

func (s *Service) beginQueueUnit(ctx context.Context, kind string) (context.Context, func(), bool) {
	if kind != "cache_verification" && kind != "download_cleanup" && kind != "distribution_verification" {
		return s.beginBackgroundUnit(ctx, kind, 15*time.Second)
	}
	return s.beginCachePayloadUnit(ctx, kind)
}

func (s *Service) beginSourceSweepUnit(ctx context.Context, kind string) (context.Context, func(), bool) {
	if kind == "cache_cleanup" {
		return s.beginCachePayloadUnit(ctx, kind)
	}
	if retentionCleanupKind(kind) {
		unit, release, admitted := s.beginBackgroundUnit(ctx, kind, historyCleanupRangeBudget)
		if !admitted {
			return unit, release, false
		}
		return withHistoryCleanupRequestBudget(unit, historyCleanupStorageRequests), release, true
	}
	return s.beginBackgroundUnit(ctx, kind, 15*time.Second)
}

func (s *Service) beginCachePayloadUnit(ctx context.Context, kind string) (context.Context, func(), bool) {
	if s.recoveryBlocked() {
		s.observeQueueDependency(kind)
		return nil, nil, false
	}
	return s.beginAdmittedUnit(ctx, kind, s.cacheVerificationBudget(), func(admission context.Context) (func(), bool) {
		if s.recoveryBlocked() {
			return nil, false
		}
		return s.acquireSolidificationSlot(admission, false)
	})
}

func (s *Service) cacheVerificationBudget() time.Duration {
	if budget := s.cfg.Limits.DownloadFinalizeTimeout; budget > 0 {
		return budget
	}
	return 300 * time.Second
}

func queueClaimDuration(ctx context.Context, kind string) time.Duration {
	if kind == "cache_verification" || kind == "download_cleanup" || kind == "distribution_verification" {
		if deadline, ok := ctx.Deadline(); ok {
			return time.Until(deadline) + 10*time.Second
		}
	}
	return time.Minute
}
