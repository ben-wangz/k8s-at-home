package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
)

func inventoryAdminPath(path string) bool {
	return path == "/api/v1/images" || strings.HasPrefix(path, "/api/v1/images/") || path == "/api/v1/cache/objects"
}

func (s *Service) serveInventory(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	q, err := parseInventoryQuery(request)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	if s.inventory == nil {
		writeInventoryError(writer, storage.ErrUnavailable)
		return
	}
	stale := s.inventoryConfig().StaleAfter
	var result any
	switch q.Kind {
	case "images", "platforms":
		result, err = s.inventory.Images(ctx, q, s.cursorKey, stale)
	case "objects", "cache":
		result, err = s.inventory.Objects(ctx, q, s.cursorKey, stale)
	case "summary":
		result, err = s.inventory.Snapshot(ctx, q.SnapshotID, stale)
	case "detail":
		var snapshot inventory.Snapshot
		snapshot, err = s.inventory.Snapshot(ctx, q.SnapshotID, stale)
		if err == nil {
			var image inventory.Image
			image, err = s.inventory.Image(ctx, snapshot, q.ImageID)
			result = inventory.Detail{Snapshot: snapshot, Image: image}
		}
	}
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func parseInventoryQuery(request *http.Request) (inventory.Query, error) {
	values := request.URL.Query()
	q := inventory.Query{Scope: values.Get("scope"), SnapshotID: values.Get("snapshotID"), Limit: 100, Token: values.Get("token"), Filters: values, Kind: "images"}
	if q.Scope == "" {
		q.Scope = "reference"
	}
	if q.Scope != "reference" && q.Scope != "platform" {
		return q, inventory.ErrInvalid
	}
	if q.SnapshotID != "" && !inventory.ValidID(q.SnapshotID) {
		return q, inventory.ErrInvalid
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			return q, inventory.ErrInvalid
		}
		q.Limit = limit
	}
	allowed := []string{"scope", "limit", "token", "snapshotID", "category", "authority", "repository", "reference", "digest", "platform", "cacheState", "executionState"}
	path := request.URL.Path
	if path == "/api/v1/cache/objects" {
		q.Kind = "cache"
		allowed = []string{"limit", "token", "snapshotID", "authority", "repository", "digest", "cacheState", "associated", "cleanupPending"}
	}
	if strings.HasPrefix(path, "/api/v1/images/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/v1/images/"), "/")
		q.ImageID = parts[0]
		q.Kind = "detail"
		if q.ImageID == "summary" && len(parts) == 1 {
			q.Kind = "summary"
			allowed = []string{"snapshotID"}
		} else {
			if !inventory.ValidID(q.ImageID) || len(parts) > 2 {
				return q, inventory.ErrInvalid
			}
			allowed = []string{"snapshotID"}
			if len(parts) == 2 {
				q.Kind = parts[1]
				allowed = []string{"snapshotID", "limit", "token"}
				if q.Kind != "platforms" && q.Kind != "objects" {
					return q, inventory.ErrInvalid
				}
			}
		}
	}
	if !onlyQueryKeys(values, allowed...) {
		return q, inventory.ErrInvalid
	}
	for _, v := range values {
		if len(v) != 1 || len(v[0]) > 4096 || v[0] == "" {
			return q, inventory.ErrInvalid
		}
	}
	if !validInventoryEnums(values.Get("category"), values.Get("cacheState"), values.Get("executionState")) {
		return q, inventory.ErrInvalid
	}
	for _, name := range []string{"associated", "cleanupPending"} {
		if v := values.Get(name); v != "" && v != "true" && v != "false" {
			return q, inventory.ErrInvalid
		}
	}
	if d := values.Get("digest"); d != "" && artifactDigest(d) != nil {
		return q, inventory.ErrInvalid
	}
	if p := values.Get("platform"); p != "" {
		parts := strings.Split(p, "/")
		if len(parts) < 2 || len(parts) > 3 {
			return q, inventory.ErrInvalid
		}
		for _, part := range parts {
			if part == "" {
				return q, inventory.ErrInvalid
			}
		}
	}
	return q, nil
}

func validInventoryEnums(category, cache, execution string) bool {
	member := func(value, set string) bool { return value == "" || strings.Contains("|"+set+"|", "|"+value+"|") }
	return member(category, "solidified|cached_not_started|solidifying|failed|cancelled|cached_start_unknown|waiting_cache|not_scheduled|unknown") && member(cache, "complete|partial|none|unknown") && member(execution, "not_scheduled|idle|queued|running|retry_wait|waiting")
}

func writeInventoryError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inventory.ErrInvalid):
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "inventory query or page token is invalid")
	case errors.Is(err, inventory.ErrExpired):
		writeAdminError(writer, http.StatusGone, "EXPIRED", "inventory snapshot or page token expired; restart pagination")
	case errors.Is(err, storage.ErrNotFound):
		writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "inventory image does not exist")
	default:
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "inventory snapshot could not be read")
	}
}
