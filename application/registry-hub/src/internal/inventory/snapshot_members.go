package inventory

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

type snapshotMember struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
}

type PlatformRequest struct {
	Identity
	ObjectRef
}

func (w *Writer) HasImage(ctx context.Context, id string) (bool, error) {
	member, _, err := Read[snapshotMember](ctx, w.r.Store, w.key("members/"+id+".json"))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err == nil && (member.ID != id || member.Scope != "reference" && member.Scope != "platform") {
		err = storage.ErrCorruptObject
	}
	return err == nil, err
}

func (w *Writer) captureOpen(ctx context.Context) error {
	_, _, err := Read[assemblyState](ctx, w.r.Store, w.key("assembly.json"))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrIncomplete
}

func (w *Writer) RequirePlatform(ctx context.Context, identity Identity, ref ObjectRef) error {
	id := ID("platform", identity.Authority, identity.Repository, ref.Digest)
	ready, err := w.HasImage(ctx, id)
	if err != nil || ready {
		return err
	}
	if err := w.captureOpen(ctx); err != nil {
		return err
	}
	identity.Reference = ref.Digest
	return Update(ctx, w.r.Store, w.key("pending/"+id+".json"), func(request *PlatformRequest) error {
		if request.Digest == "" {
			*request = PlatformRequest{Identity: identity, ObjectRef: ref}
		}
		if request.Identity != identity || request.Digest != ref.Digest {
			return storage.ErrCorruptObject
		}
		return nil
	})
}

// Completed requests are removed so dependencies inserted behind the scan cursor are revisited.
func (w *Writer) PendingPlatforms(ctx context.Context, token string, limit int) ([]PlatformRequest, []string, string, error) {
	entries, next, err := storage.Scan(ctx, w.r.Store, w.key("pending/"), token, limit)
	if err != nil {
		return nil, nil, "", err
	}
	requests := make([]PlatformRequest, 0, len(entries))
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		request, _, err := Read[PlatformRequest](ctx, w.r.Store, entry.Key)
		if err != nil {
			return nil, nil, "", err
		}
		if entry.Key != w.key("pending/"+ID("platform", request.Authority, request.Repository, request.Digest)+".json") {
			return nil, nil, "", storage.ErrCorruptObject
		}
		requests = append(requests, request)
		keys = append(keys, entry.Key)
	}
	return requests, keys, next, nil
}

func (w *Writer) CompletePlatform(ctx context.Context, key string) error {
	return w.r.Store.Delete(ctx, key)
}

func (w *Writer) validatePlatform(ctx context.Context, id string) error {
	member, _, err := Read[snapshotMember](ctx, w.r.Store, w.key("members/"+id+".json"))
	if errors.Is(err, storage.ErrNotFound) {
		return ErrIncomplete
	}
	if err != nil {
		return err
	}
	if member.ID != id || member.Scope != "platform" {
		return ErrIncomplete
	}
	image, _, err := Read[Image](ctx, w.r.Store, w.key("entries/"+id+".json"))
	if err == nil && (image.ID != id || image.Scope != "platform") {
		err = storage.ErrCorruptObject
	}
	return err
}

func (w *Writer) validateLinks(ctx context.Context, image Image) error {
	if image.Scope == "reference" && image.ResolvedDigest != "" {
		if err := w.validatePlatform(ctx, ID("platform", image.Authority, image.Repository, image.ResolvedDigest)); err != nil {
			return err
		}
	}
	pages, _, err := Read[int](ctx, w.r.Store, w.key("platforms/"+image.ID+"/count.json"))
	if err != nil || pages < 0 {
		return errors.Join(err, ErrIncomplete)
	}
	for page := 0; page < pages; page++ {
		ids, _, err := Read[[]string](ctx, w.r.Store, w.platformPageKey(image.ID, page))
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := w.validatePlatform(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}
