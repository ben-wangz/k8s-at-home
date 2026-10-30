package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

type cacheStorageMetadata struct {
	Authority  string   `json:"authority"`
	Repository string   `json:"repository"`
	Digest     string   `json:"digest"`
	MediaTypes []string `json:"mediaTypes"`
}

func (s *Service) inspectCacheStorageObject(ctx context.Context, entry storage.Entry, object *storageObject) {
	parts := strings.Split(entry.Key, "/")
	if len(parts) < 4 {
		return
	}
	identity := parts[2]
	metadata, _, err := inventory.Read[cacheStorageMetadata](ctx, s.store, "cache/objects/"+identity+"/representation-index.json")
	if err != nil || cacheStorageIdentity(metadata.Authority, metadata.Repository, metadata.Digest) != identity || len(metadata.MediaTypes) == 0 {
		object.ReclaimReason = "cache_identity_or_representation_evidence_missing"
		return
	}
	object.Authority, object.Repository, object.Digest = metadata.Authority, metadata.Repository, metadata.Digest
	object.AssociationState = "known"
	object.Cleaner = "cache_cleanup"
	inspection, err := s.cache.InspectAny(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaTypes[0])
	if err != nil {
		object.ReclaimReason = "cache_" + diagnosticReason(err)
		return
	}
	object.CleanupState = inspection.State
	if len(metadata.MediaTypes) > 1 {
		object.ReclaimReason = "multiple_representations_require_individual_expiry_validation"
		return
	}
	if !inspection.Present || inspection.Unknown {
		object.ReclaimReason = "cache_commit_or_version_unverified"
		return
	}
	active, err := s.tasks.Leases().Active(ctx, "objects/"+objectLeaseKey(metadata.Authority, metadata.Repository, metadata.Digest))
	if err != nil {
		object.ReclaimReason = "lease_" + diagnosticReason(err)
		return
	}
	if active {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "active", "blocked", "active_object_lease"
		return
	}
	pins, _, err := storage.Scan(ctx, s.store, "cache/readers/"+identity+"/", "", 1)
	if err != nil {
		object.ReclaimReason = "readers_" + diagnosticReason(err)
		return
	}
	if len(pins) > 0 {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "potential_reader", "blocked", "reader_pin_requires_expiry_validation"
		return
	}
	refs, _, err := s.records.ListCacheProtectionReferences(ctx, metadata.Digest, "", 1)
	if err != nil {
		object.ReclaimReason = "protection_" + diagnosticReason(err)
		return
	}
	if len(refs) > 0 {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "potential_solidification", "blocked", "task_protection_reference_requires_validation"
		return
	}
	intent, err := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest})
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		object.ReclaimReason = "intent_" + diagnosticReason(err)
		return
	}
	if err == nil && intent.State != tasks.IntentAnchored && intent.State != tasks.IntentVerified && intent.State != tasks.IntentFailed && intent.State != tasks.IntentCancelled {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "active_intent", "blocked", "solidification_needs_cache"
		return
	}
	if !s.cacheProtectionReady.Load() {
		object.ReclaimReason = "cache_protection_recovery_incomplete"
		return
	}
	pending, err := s.retainUnprojectedManifest(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaTypes[0], inspection.VerifiedAt)
	if err != nil || pending {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "projection", "blocked", "manifest_projection_not_confirmed"
		return
	}
	object.ProtectionState = "none_observed"
	object.ReclaimState, object.ReclaimReason = "blocked", "cache_retention_or_cleanup_deadline_not_confirmed"
	deadline, err := s.cacheStorageDeadline(ctx, identity)
	if err == nil && !deadline.IsZero() && deadline.Before(time.Now()) {
		object.ReclaimState, object.ReclaimReason = "candidate", "expired_cache_requires_cleaner_guard_revalidation"
	}
}

func (s *Service) cacheStorageDeadline(ctx context.Context, identity string) (time.Time, error) {
	entries, next, err := storage.Scan(ctx, s.store, "cache/objects/"+identity+"/representations/", "", 2)
	if err != nil {
		return time.Time{}, err
	}
	if len(entries) != 1 || next != "" {
		return time.Time{}, nil
	}
	metadata, _, err := inventory.Read[struct {
		CleanupAfter time.Time `json:"cleanupAfter"`
		VerifiedAt   time.Time `json:"verifiedAt"`
	}](ctx, s.store, entries[0].Key)
	if !metadata.CleanupAfter.IsZero() {
		return metadata.CleanupAfter, err
	}
	if !metadata.VerifiedAt.IsZero() {
		return metadata.VerifiedAt.Add(24 * time.Hour), err
	}
	return time.Time{}, err
}
