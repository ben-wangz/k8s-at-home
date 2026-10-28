package app

import (
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/inventory"

	"github.com/danielgtaylor/huma/v2"
)

func adminInventoryOperations(api huma.API) []huma.Operation {
	imagePage := adminJSONResponse("Immutable inventory image page with source coverage and freshness.", adminSchema[inventory.ImagePage](api))
	objectPage := adminJSONResponse("Immutable inventory object page; bytes are logical scope bytes.", adminSchema[inventory.ObjectPage](api))
	params := []*huma.Param{adminStringQueryParam("snapshotID", "Optional retained immutable snapshot ID.", false)}
	page := append([]*huma.Param{}, params...)
	page = append(page, adminIntegerQueryParam("limit", "Page size (default 100).", 1, 1000, 100), adminStringQueryParam("token", "Signed cursor bound to snapshot and filters; expires after 15 minutes.", false))
	images := append([]*huma.Param{}, page...)
	images = append(images, adminStringQueryParam("scope", "Inventory scope (default reference).", false, "reference", "platform"))
	for _, name := range []string{"authority", "repository", "reference", "digest", "platform", "category", "cacheState", "executionState"} {
		images = append(images, adminStringQueryParam(name, "Exact "+name+" filter.", false))
	}
	cache := append([]*huma.Param{}, page...)
	for _, name := range []string{"authority", "repository", "digest", "cacheState", "associated", "cleanupPending"} {
		cache = append(cache, adminStringQueryParam(name, "Exact "+name+" filter.", false))
	}
	operation := func(path, id, title string, response *huma.Response, query []*huma.Param) huma.Operation {
		return adminOperation(api, http.MethodGet, path, id, title, auth.TaskAdmin, adminResponses(api, map[string]*huma.Response{"200": response}, http.StatusBadRequest, http.StatusNotFound, http.StatusGone, http.StatusServiceUnavailable), query)
	}
	return []huma.Operation{
		operation("/api/v1/images", "listImages", "List image inventory", imagePage, images),
		operation("/api/v1/images/summary", "summarizeImages", "Summarize inventory classifications", adminJSONResponse("Counts by scope and category in the declared discoverable range.", adminSchema[inventory.Snapshot](api)), params),
		operation("/api/v1/images/{imageID}", "getImage", "Get inventory image", adminJSONResponse("Image evidence and lifecycle.", adminSchema[inventory.Detail](api)), append([]*huma.Param{adminPathParam("imageID", "Inventory image ID.")}, params...)),
		operation("/api/v1/images/{imageID}/platforms", "listImagePlatforms", "List child manifests including artifacts", imagePage, append([]*huma.Param{adminPathParam("imageID", "Inventory image ID.")}, page...)),
		operation("/api/v1/images/{imageID}/objects", "listImageObjects", "List image closure objects", objectPage, append([]*huma.Param{adminPathParam("imageID", "Inventory image ID.")}, page...)),
		operation("/api/v1/cache/objects", "listCacheObjects", "List cache objects including orphan blobs", objectPage, cache),
	}
}
