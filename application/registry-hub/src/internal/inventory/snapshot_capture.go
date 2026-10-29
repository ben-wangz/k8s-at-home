package inventory

import (
	"context"
	"fmt"
	"registry-hub/internal/storage"
)

type CaptureProgress struct {
	Phase  int `json:"phase"`
	Offset int `json:"offset"`
}

// CaptureImage publishes the ready marker only after every immutable payload page.
func (w *Writer) CaptureImage(ctx context.Context, image *Image, objects []Object, platforms []string, p *CaptureProgress) (bool, error) {
	member := snapshotMember{ID: image.ID, Scope: image.Scope}
	switch p.Phase {
	case 0:
		ready, err := w.HasImage(ctx, image.ID)
		if err != nil || ready {
			return ready, err
		}
		if err := w.captureOpen(ctx); err != nil {
			return false, err
		}
		if err := w.r.Immutable(ctx, w.key("captures/"+image.ID+".json"), member); err != nil {
			return false, err
		}
		p.Phase++
	case 1:
		if err := w.r.Immutable(ctx, w.key("entries/"+image.ID+".json"), image); err != nil {
			return false, err
		}
		p.Phase++
	case 2:
		prefix := "objects/" + image.ID
		if p.Offset < len(objects) {
			end := min(p.Offset+PageSize, len(objects))
			if err := w.r.Immutable(ctx, w.key(fmt.Sprintf("%s/%08d.json", prefix, p.Offset/PageSize)), objects[p.Offset:end]); err != nil {
				return false, err
			}
			p.Offset = end
			return false, nil
		}
		if err := w.r.Immutable(ctx, w.key(prefix+"/count.json"), (len(objects)+PageSize-1)/PageSize); err != nil {
			return false, err
		}
		p.Offset, p.Phase = 0, 3
	case 3:
		if p.Offset < len(platforms) {
			end := min(p.Offset+PageSize, len(platforms))
			if err := w.r.Immutable(ctx, w.platformPageKey(image.ID, p.Offset/PageSize), platforms[p.Offset:end]); err != nil {
				return false, err
			}
			p.Offset = end
			return false, nil
		}
		if err := w.r.Immutable(ctx, w.key("platforms/"+image.ID+"/count.json"), (len(platforms)+PageSize-1)/PageSize); err != nil {
			return false, err
		}
		p.Phase++
	case 4:
		err := w.r.Immutable(ctx, w.key("members/"+image.ID+".json"), member)
		return err == nil, err
	default:
		return false, storage.ErrCorruptObject
	}
	return false, nil
}
