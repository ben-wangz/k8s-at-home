package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"registry-hub/internal/storage"
)

type assemblyMember struct {
	Image  Image    `json:"image"`
	Phase  int      `json:"phase"`
	Pages  int      `json:"pages"`
	Page   int      `json:"page"`
	IDs    []string `json:"ids,omitempty"`
	Offset int      `json:"offset"`
}

func (w *Writer) AssembleStep(ctx context.Context, limit int) (bool, error) {
	if limit < 1 {
		return false, ErrInvalid
	}
	if err := w.requireNoPending(ctx); err != nil {
		return false, err
	}
	key := w.key("assembly.json")
	state, version, err := Read[assemblyState](ctx, w.r.Store, key)
	if errors.Is(err, storage.ErrNotFound) {
		if err = Update(ctx, w.r.Store, key, func(current *assemblyState) error {
			if current.Revision == 0 {
				*current = assemblyState{Revision: 1, Pages: map[string]int{}, Counts: map[string]int64{}}
			}
			return nil
		}); err != nil {
			return false, err
		}
		state, version, err = Read[assemblyState](ctx, w.r.Store, key)
	}
	if err != nil {
		return false, err
	}
	if state.Revision == 0 || state.Phase < 0 || state.Phase > 2 || state.Pages == nil || state.Counts == nil {
		return false, storage.ErrCorruptObject
	}
	w.Snapshot.Pages, w.Snapshot.Counts = maps.Clone(state.Pages), maps.Clone(state.Counts)
	w.Snapshot.UniqueManifests = state.UniqueManifests
	if state.Phase == 2 {
		return true, nil
	}
	w.images, w.objects = state.Images, state.Objects
	if w.images == nil {
		w.images = map[string][]Image{}
	}
	if err := w.advanceAssembly(ctx, &state, limit); err != nil {
		return false, err
	}
	state.Revision++
	state.Pages, state.Counts, state.UniqueManifests = w.Snapshot.Pages, w.Snapshot.Counts, w.Snapshot.UniqueManifests
	state.Images, state.Objects = w.images, w.objects
	err = w.saveAssemblyStep(ctx, key, version, state)
	return state.Phase == 2, err
}

func (w *Writer) saveAssemblyStep(ctx context.Context, key, version string, state assemblyState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 || version == "" {
		return storage.ErrCorruptObject
	}
	if guard, ok := ctx.Value(writeGuardKey{}).(func(context.Context) error); ok {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	_, err = w.r.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrUnknownCommit) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	actual, _, readErr := Read[assemblyState](ctx, w.r.Store, key)
	encoded, encodeErr := json.Marshal(actual)
	if readErr == nil && encodeErr == nil && bytes.Equal(encoded, data) {
		return nil
	}
	return errors.Join(err, readErr, encodeErr)
}

func (w *Writer) advanceAssembly(ctx context.Context, state *assemblyState, limit int) error {
	if state.Member != nil {
		done, err := w.advanceAssemblyLinks(ctx, state.Member)
		if err != nil || !done {
			return err
		}
		image := state.Member.Image
		w.images[image.Scope] = append(w.images[image.Scope], image)
		w.Snapshot.Counts[image.Scope+":"+image.Category]++
		if image.Scope == "platform" {
			w.Snapshot.UniqueManifests++
		}
		if len(w.images[image.Scope]) == PageSize {
			if err := w.flushImages(ctx, image.Scope); err != nil {
				return err
			}
		}
		state.Member = nil
		state.Cursor.Advance()
		return nil
	}
	prefix := "captures/"
	if state.Phase == 1 {
		prefix = "cache-entries/"
	}
	if state.Cursor == nil {
		state.Cursor = &Cursor{Token: state.Token}
	}
	entry, ok, err := state.Cursor.Peek(ctx, w.r.Store, w.key(prefix), limit)
	if err != nil {
		return err
	}
	if ok {
		if state.Phase == 0 {
			return w.startAssemblyMember(ctx, state, entry.Key)
		}
		if err := w.assembleEntry(ctx, entry.Key, 1); err != nil {
			return err
		}
		state.Cursor.Advance()
	}
	state.Token = state.Cursor.Token
	if state.Cursor.Done {
		if err := w.Flush(ctx); err != nil {
			return err
		}
		state.Phase++
		state.Token, state.Cursor = "", nil
	}
	return nil
}

func (w *Writer) startAssemblyMember(ctx context.Context, state *assemblyState, key string) error {
	member, _, err := Read[snapshotMember](ctx, w.r.Store, key)
	if err != nil {
		return err
	}
	ready, err := w.HasImage(ctx, member.ID)
	if err != nil {
		return err
	}
	if !ready {
		return ErrIncomplete
	}
	image, _, err := Read[Image](ctx, w.r.Store, w.key("entries/"+member.ID+".json"))
	if err != nil {
		return err
	}
	if member.ID != image.ID || member.Scope != image.Scope || image.Scope != "reference" && image.Scope != "platform" {
		return storage.ErrCorruptObject
	}
	state.Member = &assemblyMember{Image: image}
	return nil
}

func (w *Writer) advanceAssemblyLinks(ctx context.Context, p *assemblyMember) (bool, error) {
	switch p.Phase {
	case 0:
		if p.Image.Scope == "reference" && p.Image.ResolvedDigest != "" {
			if err := w.validatePlatform(ctx, ID("platform", p.Image.Authority, p.Image.Repository, p.Image.ResolvedDigest)); err != nil {
				return false, err
			}
		}
		p.Phase++
	case 1:
		pages, _, err := Read[int](ctx, w.r.Store, w.key("platforms/"+p.Image.ID+"/count.json"))
		if err != nil || pages < 0 {
			if err != nil {
				return false, err
			}
			return false, ErrIncomplete
		}
		p.Pages, p.Phase = pages, 2
	case 2:
		if p.Offset < len(p.IDs) {
			if err := w.validatePlatform(ctx, p.IDs[p.Offset]); err != nil {
				return false, err
			}
			p.Offset++
			return false, nil
		}
		if p.Page == p.Pages {
			return true, nil
		}
		ids, _, err := Read[[]string](ctx, w.r.Store, w.platformPageKey(p.Image.ID, p.Page))
		if err != nil {
			return false, err
		}
		p.IDs, p.Offset, p.Page = ids, 0, p.Page+1
	default:
		return false, storage.ErrCorruptObject
	}
	return false, nil
}
