package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) serveExecutionFacts(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/v1/images/"), "/execution-facts")
	values, cursor, limit, err := s.diagnosticQuery(request, "digest", "generation")
	if err != nil || !inventory.ValidID(id) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if raw := values.Get("generation"); raw != "" {
		if _, parseErr := strconv.ParseUint(raw, 10, 64); parseErr != nil {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
	}
	prefix := executionIndexPrefix(id)
	if values.Get("digest") != "" && values.Get("generation") != "" {
		generation, _ := strconv.ParseUint(values.Get("generation"), 10, 64)
		prefix = executionScopePrefix(id, values.Get("digest"), generation)
	}
	entries, next, err := storage.Scan(ctx, s.store, prefix, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := executionFactPage{diagnosticPage: diagnosticPage[executionProjectionFact]{Items: []executionProjectionFact{}, ObservedAt: time.Now().UTC(), Consistency: "durable source links; live work state with compact terminal facts", Scanned: len(entries)}, Coverage: "root sources indexed before execution; blob sources indexed from durable demands and generation-qualified graph nodes; verified compact terminal facts survive raw receipt cleanup; legacy and unreconciled associations remain unknown; absence does not prove never started"}
	for _, entry := range entries {
		link, _, err := inventory.Read[executionSourceLink](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if link.ImageID != id {
			writeInventoryError(writer, storage.ErrCorruptObject)
			return
		}
		if values.Get("digest") != "" && values.Get("digest") != link.Digest || values.Get("generation") != "" && values.Get("generation") != strconv.FormatUint(link.Generation, 10) {
			continue
		}
		fact, err := s.readExecutionFact(ctx, link)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		page.Items = append(page.Items, fact)
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
