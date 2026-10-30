package app

import (
	"context"
	"net/http"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"strconv"
	"strings"
	"time"
)

func (u *uploadSessions) recordUploadCompletion(ctx context.Context, state uploadSession, result, digest, rangeHeader string) {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	view := uploadTransferView(state)
	view.TransferState = result
	view.ProgressAt = time.Now().UTC()
	view.FinishedAt = view.ProgressAt
	view.Digest = digest
	if result == "complete" {
		view.TransferredBytes = nil
	}
	if start, end, ok := strings.Cut(rangeHeader, "-"); ok && start == "0" {
		if n, err := strconv.ParseInt(end, 10, 64); err == nil && n > 0 && n < int64(^uint64(0)>>1) {
			n++
			view.TransferredBytes = &n
		}
	}
	if result == "complete" {
		view.TotalBytes = view.TransferredBytes
	}
	_ = inventory.Update(bounded, u.store, diagnostics.Prefix+"upload-receipts/"+state.ID+".json", func(current *transferView) error { *current = view; return nil })
}

func uploadTransferView(state uploadSession) transferView {
	view := transferView{ID: inventory.ID("client_upload", "", state.Repository, "", state.ID), Kind: "client_upload", workAssociations: workAssociations{Repository: state.Repository, SessionID: state.ID}, StartedAt: state.CreatedAt, ProgressAt: state.UpdatedAt, ObservedAt: time.Now().UTC(), TransferState: "in_progress", CacheCommitState: "not_applicable", CleanupState: "unknown", QuotaReleaseState: "not_applicable", EvidenceSource: "persisted upload session or sanitized completion receipt"}
	view.TransferID = view.ID
	view.TransferredBytes = state.ReceivedBytes
	return view
}

func acceptedUploadBytes(value string) (int64, bool) {
	start, end, ok := strings.Cut(value, "-")
	if !ok || start != "0" {
		return 0, false
	}
	n, err := strconv.ParseInt(end, 10, 64)
	if err != nil || n <= 0 || n == int64(^uint64(0)>>1) {
		return 0, false
	}
	return n + 1, true
}

func uploadCompletionResult(method string, status int) string {
	if status == http.StatusCreated {
		return "complete"
	}
	if method == http.MethodDelete {
		return "cancelled"
	}
	return "unknown"
}
