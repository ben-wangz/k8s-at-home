package app

import (
	"context"
	"registry-hub/internal/inventory"
	"strings"
)

func (s *Service) inspectAssociatedMetadata(ctx context.Context, key string, object *storageObject) bool {
	matched := false
	for _, prefix := range []string{"tasks/state/", "solidification/graphs/", "solidification/blobs/intents/", "solidification/blobs/owners/", "uploads/sessions/", "diagnostics/v1/events/", "diagnostics/v1/transfers/", "diagnostics/v1/upload-receipts/"} {
		matched = matched || strings.HasPrefix(key, prefix)
	}
	if !matched || !strings.HasSuffix(key, ".json") {
		return false
	}
	record, _, err := inventory.Read[struct {
		ID, TaskID, RootTaskID, WorkID, IntentID, TransferID, SessionID, ImageID      string
		Authority, Repository, Reference, Digest, RootDigest, BlobDigest, ChildDigest string
		Identity                                                                      struct{ Authority, Repository, Digest string }
	}](ctx, s.store, key)
	if err != nil {
		object.ReclaimReason = "metadata_association_" + diagnosticReason(err)
		return true
	}
	object.Authority, object.Repository, object.Digest = record.Authority, record.Repository, record.Digest
	if record.Identity.Authority != "" {
		object.Authority, object.Repository, object.Digest = record.Identity.Authority, record.Identity.Repository, record.Identity.Digest
	}
	object.TaskID, object.IntentID, object.TransferID, object.SessionID, object.WorkID = record.TaskID, record.IntentID, record.TransferID, record.SessionID, record.WorkID
	object.ImageID = record.ImageID
	if record.RootTaskID != "" {
		object.TaskID = record.RootTaskID
	}
	if record.Reference != "" {
		object.ImageID = (inventory.Identity{Authority: object.Authority, Repository: object.Repository, Reference: record.Reference}).ID()
	}
	if strings.HasPrefix(key, "tasks/state/") {
		object.TaskID = record.ID
		object.Digest = record.RootDigest
		if object.Digest == "" {
			object.Digest = record.BlobDigest
		}
	}
	if strings.HasPrefix(key, "solidification/blobs/intents/") {
		object.IntentID = record.ID
	}
	if strings.HasPrefix(key, "uploads/sessions/") {
		object.SessionID = record.ID
		object.TransferID = inventory.ID("client_upload", "", object.Repository, "", record.ID)
	}
	if object.Digest == "" {
		object.Digest = record.ChildDigest
	}
	if object.TaskID != "" || object.IntentID != "" || object.TransferID != "" || object.SessionID != "" || object.WorkID != "" || object.Repository != "" {
		object.AssociationState = "known"
	}
	return true
}
