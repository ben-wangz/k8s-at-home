package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
	"time"
)

func (s *solidifier) readDistributionManifest(ctx context.Context, task *tasks.Task, reference contentRef) ([]byte, contentRef, error) {
	if err := s.service.inventoryStart(ctx, task, reference.digest); err != nil {
		return nil, contentRef{}, err
	}
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/manifests/" + url.PathEscape(reference.digest)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return nil, contentRef{}, temporary("distribution", err)
	}
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodGet, route.EndpointManifest)
		_ = response.Body.Close()
		if miss {
			return nil, contentRef{}, storage.ErrNotFound
		}
		return nil, contentRef{}, temporary("distribution", errors.New("Distribution returned an untrusted manifest miss"))
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, contentRef{}, temporary("distribution", fmt.Errorf("Distribution manifest GET returned status %d", response.StatusCode))
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	mediaType := response.Header.Get("Content-Type")
	headerDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if readErr != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes || response.ContentLength >= 0 && int64(len(data)) != response.ContentLength || artifact.VerifyDigest(reference.digest, data) != nil || headerDigest != "" && headerDigest != reference.digest {
		return nil, contentRef{}, permanent("distribution_invalid", errors.New("existing Distribution manifest failed digest or size verification"))
	}
	if reference.size > 0 && reference.size != int64(len(data)) || reference.mediaType != "" && !sameMediaType(reference.mediaType, mediaType) {
		return nil, contentRef{}, permanent("distribution_invalid", errors.New("existing Distribution manifest differs from its descriptor"))
	}
	return data, contentRef{digest: reference.digest, mediaType: mediaType, size: int64(len(data))}, nil
}

func (s *solidifier) admitManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest, requestedBytes int64, maxSessions int, maxBytes int64, reopenAnchored bool) (bool, error) {
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
	if manifest.scopeBytes > maxBytes {
		return false, permanent("capacity_limit", errors.New("manifest scope exceeds the unanchored Distribution byte limit"))
	}
	_, alreadyAdmitted, err := s.gate.protectionSession(ctx, manifest.sessionID)
	if err != nil {
		return false, temporary("protection_state", err)
	}
	if !alreadyAdmitted {
		existing, readErr := s.gate.readProtectionSession(ctx, manifest.sessionID)
		if readErr == nil && (existing.State == "anchored" || existing.State == "released" && existing.TerminalReason == "anchored") {
			if !reopenAnchored {
				clearQuotaWaitHints(task)
				return false, nil
			}
		}
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return false, temporary("protection_state", readErr)
		}
	}
	oldSessions := make(map[string]struct{})
	var replaceableBytes int64
	replaceableSlots := 0
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return false, temporary("task_state", err)
		}
		if intent.ProtectionSessionID == "" || intent.ProtectionSessionID == manifest.sessionID {
			continue
		}
		session, active, err := s.gate.protectionSession(ctx, intent.ProtectionSessionID)
		if err != nil {
			return false, temporary("protection_state", err)
		}
		directScope, scopeErr := protectionScopeID(task.Authority, task.Repository, blob.digest, task.Generation)
		if scopeErr != nil {
			return false, permanent("task_state", scopeErr)
		}
		if active && session.ScopeID == directScope {
			_, seen := oldSessions[session.ID]
			oldSessions[session.ID] = struct{}{}
			if !seen && session.ReservationClass == "new" && (session.State == "protected" || session.State == "anchoring") && session.PendingOperations == 0 && !session.UncertainRemoteResult {
				if session.ReservedBytes > math.MaxInt64-replaceableBytes {
					return false, temporary("protection_state", errors.New("replaceable protection reservation overflows"))
				}
				replaceableBytes += session.ReservedBytes
				replaceableSlots++
			}
		}
	}
	oldIDs := make([]string, 0, len(oldSessions))
	for id := range oldSessions {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs)
	if !alreadyAdmitted {
		task.QuotaWaitBytes = manifest.scopeBytes
		task.QuotaReplaceableBytes = replaceableBytes
		task.QuotaReplaceableSlots = replaceableSlots
		turn, err := s.tasks.IsQuotaTurnAt(ctx, task.ID, "distribution_quota", markQuotaWaitPosition(task, "distribution_quota"))
		if err != nil {
			return false, temporary("task_state", err)
		}
		if !turn {
			return false, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
		}
	}
	var admissionErr error
	if reopenAnchored {
		admissionErr = s.gate.ReopenAnchoredProtection(ctx, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	} else if len(oldIDs) > 0 {
		admissionErr = s.gate.ReplaceProtectionSet(ctx, oldIDs, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	} else {
		admissionErr = s.gate.AdmitProtection(ctx, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	}
	if admissionErr != nil {
		if errors.Is(admissionErr, ErrProtectionAnchored) {
			return false, nil
		}
		if errors.Is(admissionErr, ErrProtectionCapacity) {
			task.QuotaWaitBytes = manifest.scopeBytes
			task.QuotaReplaceableBytes = replaceableBytes
			task.QuotaReplaceableSlots = replaceableSlots
			return false, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
		}
		if errors.Is(admissionErr, ErrWritesClosed) || errors.Is(admissionErr, ErrRecoveryBarrier) {
			return false, tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
		}
		if errors.Is(admissionErr, ErrProtectionBusy) {
			return false, tasks.WaitFor(tasks.WaitingDeps, "protection_transfer")
		}
		return false, temporary("protection_admission", admissionErr)
	}
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return false, temporary("task_state", err)
		}
		_, transferred := oldSessions[intent.ProtectionSessionID]
		if intent.ProtectionSessionID == "" || transferred {
			if err := s.service.records.SetBlobIntentProtectionSession(ctx, intent.ID, manifest.sessionID); err != nil {
				return false, temporary("task_state", err)
			}
			intent.ProtectionSessionID = manifest.sessionID
		}
		if intent.ProtectionSessionID == manifest.sessionID {
			err := s.tasks.ReconcileProtectionState(ctx, intent.ID, manifest.scopeID, []string{manifest.sessionID}, manifest.scopeBytes, manifest.scopeBytes)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, temporary("task_state", err)
			}
		}
	}
	if err := s.syncRootProtectionTask(ctx, task, taskLease, manifest.sessionID, requestedBytes); err != nil {
		return false, err
	}
	clearQuotaWaitHints(task)
	return true, nil
}

func clearQuotaWaitHints(task *tasks.Task) {
	task.QuotaWaitAt = time.Time{}
	task.QuotaWaitReason = ""
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
}
