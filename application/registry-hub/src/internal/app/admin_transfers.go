package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type transferHistoryPage struct {
	diagnosticPage[transferView]
	HistoryBefore time.Time `json:"historyBefore"`
	Coverage      string    `json:"coverage"`
}

func (s *Service) serveTransfers(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if request.URL.Path != "/api/v1/transfers" {
		id := strings.TrimPrefix(request.URL.Path, "/api/v1/transfers/")
		if !inventory.ValidID(id) || len(request.URL.Query()) != 0 {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		view, err := s.terminalTransferHistoryDetail(ctx, id)
		if err != nil {
			if errors.Is(err, inventory.ErrExpired) {
				writeAdminError(writer, http.StatusGone, "HISTORY_EXPIRED", "terminal transfer history is outside the configured retention window")
				return
			}
			writeInventoryError(writer, err)
			return
		}
		if _, terminal := terminalTransferTime(view); !terminal {
			attachTransferStorage(&view, s.transferStorageSnapshot(ctx))
			s.attachTransferAttempt(ctx, &view)
		}
		writeJSON(writer, http.StatusOK, view)
		return
	}
	values, cursor, limit, err := s.diagnosticQuery(request, "kind", "state", "authority", "repository", "digest", "taskID", "intentID", "sessionID", "cleanupState", "errorClass")
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	if cursor.Phase < 0 || cursor.Phase > 1 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if cursor.HistorySource == "" {
		if values.Get("token") != "" {
			cursor.HistorySource = terminalTransferHistoryLegacySource
		} else {
			cursor.HistorySource = terminalTransferHistorySource(s.cfg.Diagnostics.HistoryProjectionReadsEnabled)
		}
		cursor.HistoryBefore = time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	if cursor.Phase == 1 {
		s.listTerminalTransfers(writer, ctx, values, cursor, limit)
		return
	}
	entries, next, err := storage.Scan(ctx, s.store, diagnostics.Prefix+"transfers/", cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := transferHistoryPage{diagnosticPage: diagnosticPage[transferView]{Items: []transferView{}, ObservedAt: time.Now().UTC(), Consistency: "active transfer observations and terminal history ordered by completion time", Scanned: len(entries)}}
	physical := s.transferStorageSnapshot(ctx)
	for _, entry := range entries {
		view, _, err := inventory.Read[transferView](ctx, s.store, entry.Key)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if _, terminal := terminalTransferTime(view); terminal {
			continue
		}
		if transferHistoryMatches(view, values) {
			attachTransferStorage(&view, physical)
			s.attachTransferAttempt(ctx, &view)
			page.Items = append(page.Items, view)
		}
	}
	cursor.Storage = next
	if next != "" {
		page.NextToken = s.diagnosticToken(cursor)
		writeJSON(writer, http.StatusOK, page)
		return
	}
	cursor.Phase, cursor.Storage = 1, ""
	page.HistoryBefore = cursor.HistoryBefore
	if len(page.Items) >= limit {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	} else if err := s.appendTerminalTransferHistory(ctx, values, &cursor, limit-len(page.Items), &page); err != nil {
		writeInventoryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) listTerminalTransfers(writer http.ResponseWriter, ctx context.Context, values url.Values, cursor diagnosticCursor, limit int) {
	if cursor.HistorySource == "" {
		if cursor.Storage != "" {
			writeInventoryError(writer, inventory.ErrExpired)
			return
		}
		cursor.HistorySource, cursor.HistoryBefore = terminalTransferHistorySource(s.cfg.Diagnostics.HistoryProjectionReadsEnabled), time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	if cursor.HistorySource != terminalTransferHistoryLegacySource && cursor.HistorySource != terminalTransferHistoryIndexSource {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page := transferHistoryPage{diagnosticPage: diagnosticPage[transferView]{Items: []transferView{}, ObservedAt: time.Now().UTC(), Consistency: "terminal transfer history ordered by completion time"}, HistoryBefore: cursor.HistoryBefore, Coverage: "terminal transfer history retained for " + s.diagnosticsConfig().TerminalHistoryRetention.String()}
	if cursor.HistorySource == terminalTransferHistoryLegacySource {
		page.Coverage += "; live-source fallback remains enabled until projection backfill is verified"
	}
	if err := s.appendTerminalTransferHistory(ctx, values, &cursor, limit, &page); err != nil {
		writeInventoryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) appendTerminalTransferHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int, page *transferHistoryPage) error {
	rows, scanned, more, err := s.listTerminalTransferHistory(ctx, values, cursor, limit)
	if err != nil {
		return err
	}
	page.Scanned += scanned
	for _, record := range rows {
		page.Items = append(page.Items, record.View)
	}
	if more {
		if len(rows) > 0 && cursor.HistorySource == terminalTransferHistoryLegacySource {
			last := rows[len(rows)-1]
			cursor.HistoryAfter, cursor.HistoryKind, cursor.HistoryID, cursor.HistoryRevision = last.TerminalAt, last.View.Kind, last.ID, last.Revision
		}
		page.NextToken = s.diagnosticToken(*cursor)
	}
	return nil
}
