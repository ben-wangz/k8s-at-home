package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"slices"
	"strconv"
	"time"
)

type reservationView struct {
	ID            string    `json:"id"`
	SessionID     string    `json:"sessionID"`
	Digest        string    `json:"digest"`
	ReservedBytes int64     `json:"reservedBytes"`
	StateEvidence string    `json:"stateEvidence"`
	TransferID    string    `json:"transferID,omitempty"`
	CleanupWorkID string    `json:"cleanupWorkID,omitempty"`
	ReconcilerURL string    `json:"reconcilerURL"`
	ObservedAt    time.Time `json:"observedAt"`
	Reason        string    `json:"reason,omitempty"`
}

func (s *Service) serveReservations(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	values, cursor, limit, err := s.diagnosticQuery(request, "digest", "sessionID", "stateEvidence")
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	quota, err := s.readDiagnosticQuota(ctx)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	keys := make([]string, 0, len(quota.Reservations))
	for key := range quota.Reservations {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	offset := 0
	if cursor.Storage != "" {
		offset, err = strconv.Atoi(cursor.Storage)
	}
	if err != nil || offset < 0 || offset > len(keys) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page := diagnosticPage[reservationView]{Items: []reservationView{}, ObservedAt: time.Now().UTC(), Consistency: "live quota map (at most 128 reservations); state and cleanup evidence have independent read times"}
	for offset < len(keys) && page.Scanned < limit {
		key := keys[offset]
		r := quota.Reservations[key]
		view := reservationView{ID: diagnostics.ID("download-reservation", key), SessionID: r.Session, Digest: r.Digest, ReservedBytes: r.Bytes, StateEvidence: "unknown", ReconcilerURL: "/api/v1/workers/download_quota", ObservedAt: time.Now().UTC()}
		hash := sha256.Sum256([]byte(r.Identity + "\x00" + r.Digest))
		state, _, readErr := inventory.Read[struct {
			Session string `json:"session"`
		}](ctx, s.store, "downloads/v1/"+hex.EncodeToString(hash[:])+"/state.json")
		switch {
		case errors.Is(readErr, storage.ErrNotFound):
			view.StateEvidence, view.Reason = "state_absent", "orphan_reservation_reconciliation_pending"
		case readErr != nil:
			view.Reason = diagnosticReason(readErr)
		case state.Session == r.Session:
			view.StateEvidence = "matching_state"
		default:
			view.StateEvidence, view.Reason = "superseded_session", "reservation_requires_session_guard_validation"
		}
		transfer, _, readErr := inventory.Read[transferView](ctx, s.store, transferSessionKey(r.Session))
		if readErr == nil {
			view.TransferID, view.CleanupWorkID = transfer.ID, transfer.CleanupWorkID
		}
		offset++
		page.Scanned++
		if (values.Get("digest") == "" || values.Get("digest") == view.Digest) && (values.Get("sessionID") == "" || values.Get("sessionID") == view.SessionID) && (values.Get("stateEvidence") == "" || values.Get("stateEvidence") == view.StateEvidence) {
			page.Items = append(page.Items, view)
		}
	}
	if offset < len(keys) {
		cursor.Storage = strconv.Itoa(offset)
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
