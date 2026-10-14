package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) clearAllReferrerRepairs(ctx context.Context, routed route.Route, subject string, desired map[string]json.RawMessage) error {
	authority, repository := routeStorageIdentity(routed)
	prefix := referrerRepairSubjectPrefix(authority, repository, subject)
	token := ""
	for {
		entries, next, err := s.store.List(ctx, prefix, token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			intent, loadErr := s.loadReferrerRepair(ctx, entry.Key)
			if errors.Is(loadErr, storage.ErrNotFound) {
				continue
			}
			if loadErr != nil {
				return loadErr
			}
			if intent.Authority == authority && intent.Repository == repository && intent.Subject == subject {
				if _, err := s.advanceGeneration(ctx, authority, repository, referrerGenerationReference(subject, intent.Referrer), intent.Operation == "remove"); err != nil {
					return err
				}
				_, present := desired[intent.Referrer]
				for _, taskID := range intent.TaskIDs {
					if (intent.Operation == "add") == present {
						if err := s.tasks.RepairComplete(ctx, taskID); err != nil {
							return err
						}
					} else {
						if err := s.tasks.RepairSuperseded(ctx, taskID); err != nil {
							return err
						}
					}
				}
				if err := s.clearReferrerRepair(ctx, intent); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (s *Service) clearReferrerRepairByIdentity(ctx context.Context, authority, repository, subject, digest string) error {
	intent, err := s.loadReferrerRepair(ctx, referrerRepairKey(authority, repository, subject, digest))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.clearReferrerRepair(ctx, intent)
}

func (s *Service) runReferrerRepairs(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	token := ""
	scanPending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, next, err := s.store.List(ctx, referrerRepairPrefix, token, 100)
			if err != nil {
				s.logger.Warn("referrer repair listing failed", "error_class", "storage")
				token = ""
				scanPending = false
				continue
			}
			if len(entries) > 0 || next != "" {
				s.setRepairPending(true)
			}
			pending := false
			for _, entry := range entries {
				intent, loadErr := s.loadReferrerRepair(ctx, entry.Key)
				if loadErr != nil {
					s.logger.Warn("referrer repair state is invalid", "error_class", "storage")
					continue
				}
				if !intent.Retryable || intent.NextAttemptAt.After(time.Now()) {
					continue
				}
				pending = true
				s.setRepairPending(true)
				if runErr := s.processReferrerRepair(ctx, intent); runErr != nil {
					if errors.Is(runErr, lease.ErrHeld) || errors.Is(runErr, errSolidificationBusy) || errors.Is(runErr, context.Canceled) {
						continue
					}
					if updateErr := s.recordReferrerRepairFailure(ctx, intent, runErr); updateErr != nil {
						s.logger.Warn("referrer repair retry state update failed", "error_class", "storage")
					}
					s.logger.Warn("referrer repair retry failed", "error_class", referrerErrorClass(runErr), "retryable", referrerRetryable(runErr))
				}
			}
			token = next
			if token == "" {
				scanPending = scanPending || pending
				if !scanPending {
					s.setRepairPending(false)
				}
				scanPending = false
				continue
			}
			scanPending = scanPending || pending
		}
	}
}

func (s *Service) processReferrerRepair(ctx context.Context, intent *referrerRepairIntent) error {
	releaseSlot, ok := s.acquireSolidificationSlot(ctx, true)
	if !ok {
		s.metrics.recordRejection("solidification_concurrency")
		return errSolidificationBusy
	}
	defer releaseSlot()
	routed, err := referrerRoute(intent.Authority, intent.Repository)
	if err != nil {
		return invalidReferrer("referrer repair route is invalid")
	}
	releaseGate, err := s.gate.Enter(ctx)
	if err != nil {
		return err
	}
	defer releaseGate()
	leaseCtx, subjectLease, releaseSubject, err := s.acquireReferrerLease(ctx, routed, intent.Subject)
	if err != nil {
		return err
	}
	defer releaseSubject()
	current, err := s.generation(leaseCtx, intent.Authority, intent.Repository, referrerGenerationReference(intent.Subject, intent.Referrer))
	if err != nil {
		return err
	}
	if current.Generation != intent.Generation {
		for _, taskID := range intent.TaskIDs {
			if err := s.tasks.RepairSuperseded(leaseCtx, taskID); err != nil {
				return err
			}
		}
		return s.clearReferrerRepair(leaseCtx, intent)
	}
	native, err := s.nativeReferrersAvailable(leaseCtx, routed, intent.Subject)
	if err != nil {
		return err
	}
	if native {
		if intent.Operation == "add" {
			if err := s.publishLocalReferrerToNative(leaseCtx, routed, intent); err != nil {
				return err
			}
		} else if !intent.DeleteManifest {
			return invalidReferrer("native Referrers API does not support unlinking a referrer without deleting its manifest")
		}
	} else {
		tag := referrerFallbackTag(intent.Subject)
		tagCtx, tagLease, releaseTag, leaseErr := acquireRenewedLease(leaseCtx, s.tasks.Leases(), tagLeaseKey(intent.Authority, intent.Repository, tag), requestID(), 60*time.Second)
		if leaseErr != nil {
			return leaseErr
		}
		mutate := func(current []json.RawMessage) ([]json.RawMessage, bool, error) {
			if intent.Operation == "remove" {
				return updateReferrerDescriptors(current, json.RawMessage(`{"digest":"`+intent.Referrer+`"}`), true)
			}
			descriptor, descriptorErr := s.localReferrerDescriptor(tagCtx, routed, intent.Referrer, intent.Subject)
			if descriptorErr != nil {
				return nil, false, descriptorErr
			}
			return updateReferrerDescriptors(current, descriptor, false)
		}
		writeErr := s.writeFallbackReferrers(leaseCtx, subjectLease, tagCtx, tagLease, routed, intent.Subject, tag, mutate)
		_ = releaseTag()
		if writeErr != nil {
			return writeErr
		}
	}
	if intent.DeleteManifest {
		if err := s.deleteLocalReferrerManifest(leaseCtx, routed, intent.Referrer); err != nil {
			return err
		}
	}
	for _, taskID := range intent.TaskIDs {
		if err := s.tasks.RepairComplete(leaseCtx, taskID); err != nil {
			return err
		}
	}
	return s.clearReferrerRepair(leaseCtx, intent)
}

func referrerRoute(authority, repository string) (route.Route, error) {
	if route.ValidateRepository(repository) != nil {
		return route.Route{}, route.ErrInvalidPath
	}
	if authority == "" {
		internal, err := route.InternalName(repository)
		if err != nil {
			return route.Route{}, err
		}
		return route.Route{Repository: repository, Internal: internal}, nil
	}
	return taskRoute(authority, repository)
}
