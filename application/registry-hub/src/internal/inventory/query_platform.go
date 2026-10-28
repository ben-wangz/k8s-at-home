package inventory

import (
	"context"
	"net/url"
)

func (r *Repository) matchesInventoryImage(ctx context.Context, snapshot Snapshot, image Image, filters url.Values) (bool, error) {
	if filters.Get("platform") == "" || image.Platform != nil || image.Scope == "platform" {
		return matchesImage(image, filters), nil
	}
	other := url.Values{}
	for key, values := range filters {
		if key != "platform" {
			other[key] = values
		}
	}
	if !matchesImage(image, other) {
		return false, nil
	}
	kind := "platforms/" + image.ID
	pages, _, err := Read[int](ctx, r.Store, Prefix+"snapshots/"+snapshot.SnapshotID+"/"+kind+"/count.json")
	if err != nil {
		return false, err
	}
	for page := 0; page < pages; page++ {
		ids, err := readPage[string](ctx, r, snapshot, kind, page)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			child, err := r.Image(ctx, snapshot, id)
			if err != nil {
				return false, snapshotPageError(err)
			}
			if matchesImage(child, url.Values{"platform": {filters.Get("platform")}}) {
				return true, nil
			}
		}
	}
	return false, nil
}
