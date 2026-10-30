package app

import (
	"context"
	"net/http"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) serveTransfers(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if request.URL.Path != "/api/v1/transfers" {
		id := strings.TrimPrefix(request.URL.Path, "/api/v1/transfers/")
		if !inventory.ValidID(id) || len(request.URL.Query()) != 0 {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		view, _, err := inventory.Read[transferView](ctx, s.store, diagnostics.Prefix+"transfers/"+id+".json")
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		attachTransferStorage(&view, s.transferStorageSnapshot(ctx))
		s.attachTransferAttempt(ctx, &view)
		writeJSON(writer, http.StatusOK, view)
		return
	}
	values, cursor, limit, err := s.diagnosticQuery(request, "kind", "state", "authority", "repository", "digest", "taskID", "intentID", "sessionID", "cleanupState", "errorClass")
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	entries, next, err := storage.Scan(ctx, s.store, diagnostics.Prefix+"transfers/", cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := diagnosticPage[transferView]{Items: []transferView{}, ObservedAt: time.Now().UTC(), Consistency: "background observations; source times are preserved", Scanned: len(entries)}
	physical := s.transferStorageSnapshot(ctx)
	for _, entry := range entries {
		view, _, err := inventory.Read[transferView](ctx, s.store, entry.Key)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		fields := map[string]string{"kind": view.Kind, "state": view.TransferState, "authority": view.Authority, "repository": view.Repository, "digest": view.Digest, "taskID": view.TaskID, "intentID": view.IntentID, "sessionID": view.SessionID, "cleanupState": view.CleanupState, "errorClass": view.ErrorClass}
		matched := true
		for key, value := range fields {
			if values.Get(key) != "" && values.Get(key) != value {
				matched = false
			}
		}
		if matched {
			attachTransferStorage(&view, physical)
			s.attachTransferAttempt(ctx, &view)
			page.Items = append(page.Items, view)
		}
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
