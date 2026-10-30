package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strconv"
	"strings"
	"time"
)

func storageCategory(domain, key string) string {
	if domain == "registry" {
		switch {
		case strings.Contains(key, "/_uploads/"):
			return "registry_uploads"
		case strings.Contains(key, "/blobs/") && strings.HasSuffix(key, "/data"):
			return "registry_payloads"
		case strings.Contains(key, "/repositories/"):
			return "registry_links"
		default:
			return "registry_other"
		}
	}
	switch {
	case strings.HasPrefix(key, "cache/objects/") && strings.HasSuffix(key, "/data"):
		return "cache_payloads"
	case strings.HasPrefix(key, "cache/objects/"):
		return "cache_metadata"
	case strings.HasPrefix(key, "downloads/"):
		return "download_temporary"
	case strings.HasPrefix(key, "uploads/"):
		return "upload_sessions"
	case strings.HasPrefix(key, workqueue.Prefix+"jobs/"):
		return "background_jobs"
	case strings.HasPrefix(key, workqueue.Prefix+"receipts/"):
		return "background_receipts"
	case strings.HasPrefix(key, workqueue.Prefix):
		return "background_checkpoints"
	case strings.HasPrefix(key, diagnostics.Prefix+"events/"):
		return "diagnostic_events"
	case strings.HasPrefix(key, diagnostics.Prefix):
		return "diagnostic_metadata"
	case strings.HasPrefix(key, inventory.Prefix+"snapshots/"):
		return "inventory_snapshots"
	case strings.HasPrefix(key, inventory.Prefix):
		return "inventory_metadata"
	case strings.HasPrefix(key, "tasks/"):
		return "task_metadata"
	case strings.HasPrefix(key, "solidification/"):
		return "solidification_metadata"
	case strings.HasPrefix(key, "generations/"):
		return "generation_metadata"
	case strings.HasPrefix(key, "system/"):
		return "system_metadata"
	default:
		return "unclassified"
	}
}

func (s *Service) inspectStorageObject(ctx context.Context, domain, namespace string, entry storage.Entry) storageObject {
	object := s.inspectStorageObjectMetadata(ctx, domain, namespace, entry)
	s.inspectReclaimEvidence(ctx, entry, &object)
	return object
}

func (s *Service) inspectStorageObjectMetadata(ctx context.Context, domain, namespace string, entry storage.Entry) storageObject {
	object := storageObject{PhysicalID: diagnostics.ID(domain, namespace, entry.Key), Domain: domain, Category: storageCategory(domain, entry.Key), Version: diagnostics.ID("version", entry.Version), ObjectBytes: entry.Size, ObservedAt: time.Now().UTC(), AssociationState: "unknown", ProtectionState: "unknown", ReclaimState: "unknown", ReclaimReason: "requires_cleaner_validation", CleanupState: "unknown", EvidenceSource: "storage_list_metadata"}
	if entry.Version == "" {
		object.Version = diagnostics.ID("metadata-version", object.PhysicalID, strconv.FormatInt(entry.Size, 10), strconv.FormatInt(entry.ModTime, 10))
		object.EvidenceSource = "filesystem_stat; version is a metadata observation, not a content hash"
	}
	if entry.ModTime != 0 {
		object.ModifiedAt = time.Unix(0, entry.ModTime).UTC()
	}
	if domain == "registry" {
		parts := strings.Split(entry.Key, "/")
		for index, part := range parts {
			if part == "sha256" && index+1 < len(parts) {
				hash := parts[index+1]
				if len(hash) == 2 && index+2 < len(parts) {
					hash = parts[index+2]
				}
				if len(hash) == 64 {
					object.Digest = "sha256:" + hash
				}
			}
		}
		if object.Category == "registry_uploads" {
			object.Cleaner = "distribution_upload_purge"
			object.ReclaimReason = "upload_activity_and_purge_configuration_unknown; blob_gc_does_not_reclaim_upload_sessions"
		}
		return object
	}
	if strings.HasPrefix(entry.Key, "cache/objects/") {
		s.inspectCacheStorageObject(ctx, entry, &object)
		return object
	}
	if s.inspectAssociatedMetadata(ctx, entry.Key, &object) {
		return object
	}
	if strings.HasPrefix(entry.Key, workqueue.Prefix+"jobs/") || strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/") {
		job, _, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
		if err == nil {
			view := diagnosticJob(job)
			object.workAssociations = view.workAssociations
			object.WorkID = job.ID
			object.AssociationState = "known"
		}
		return object
	}
	if strings.HasPrefix(entry.Key, "downloads/v1/") {
		parts := strings.Split(entry.Key, "/")
		if len(parts) >= 4 {
			if len(parts) >= 5 && parts[3] == "sessions" {
				view, _, err := inventory.Read[transferView](ctx, s.store, transferSessionKey(parts[4]))
				if err == nil {
					object.workAssociations = view.workAssociations
					object.TransferID = view.ID
					object.WorkID = view.CleanupWorkID
					object.CleanupState = view.CleanupState
					object.AssociationState = "known"
				}
			}
			var state struct {
				Authority  string `json:"authority"`
				Repository string `json:"repository"`
				Digest     string `json:"digest"`
				Session    string `json:"session"`
			}
			state, _, err := inventory.Read[struct {
				Authority  string `json:"authority"`
				Repository string `json:"repository"`
				Digest     string `json:"digest"`
				Session    string `json:"session"`
			}](ctx, s.store, strings.Join(parts[:3], "/")+"/state.json")
			if err == nil && (len(parts) < 5 || parts[3] == "sessions" && parts[4] == state.Session) {
				object.Authority, object.Repository, object.Digest, object.SessionID = state.Authority, state.Repository, state.Digest, state.Session
				object.AssociationState = "known"
				object.TransferID = inventory.ID("upstream_download", state.Authority, state.Repository, state.Digest, state.Session)
			}
			object.Cleaner = "download_cleanup"
			object.ReclaimReason = "session_fence_cache_commit_and_reservation_must_be_revalidated"
		}
	}
	return object
}

func addStorageUsage(state *storageCollection, object storageObject) {
	if object.TransferID != "" && object.Category == "download_temporary" {
		if state.Snapshot.TransferTemporaryBytes == nil {
			state.Snapshot.TransferTemporaryBytes = map[string]int64{}
		}
		if _, exists := state.Snapshot.TransferTemporaryBytes[object.TransferID]; exists || len(state.Snapshot.TransferTemporaryBytes) < 4096 {
			state.Snapshot.TransferTemporaryBytes[object.TransferID] += object.ObjectBytes
		} else {
			state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "transfer_attribution_limit")
		}
	}
	addStorageAttributionUsage(state, object)
	domain := state.Snapshot.Domains[object.Domain]
	bytes, objects := int64(0), int64(0)
	if domain.ObjectBytes != nil {
		bytes = *domain.ObjectBytes
		objects = *domain.Objects
	}
	bytes += object.ObjectBytes
	objects++
	domain.ObjectBytes, domain.Objects = &bytes, &objects
	category := domain.Categories[object.Category]
	category.Domain, category.Dimension, category.Name = object.Domain, "category", object.Category
	category.ObjectBytes += object.ObjectBytes
	category.Objects++
	domain.Categories[object.Category] = category
	state.Snapshot.Domains[object.Domain] = domain
}

func appendUniqueReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
