package inventory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"registry-hub/internal/storage"
)

type assemblyState struct {
	Revision        uint64           `json:"revision"`
	Phase           int              `json:"phase"`
	Token           string           `json:"token"`
	Pages           map[string]int   `json:"pages"`
	Counts          map[string]int64 `json:"counts"`
	UniqueManifests int64            `json:"uniqueManifests"`
}

func (w *Writer) platformPageKey(id string, page int) string {
	return w.key(fmt.Sprintf("platforms/%s/%08d.json", id, page))
}

// Assembly seals capture and checkpoints pages only after their immutable writes succeed.
func (w *Writer) Assemble(ctx context.Context, limit int) (bool, error) {
	if limit < 1 {
		return false, ErrInvalid
	}
	if err := w.requireNoPending(ctx); err != nil {
		return false, err
	}
	key := w.key("assembly.json")
	if err := Update(ctx, w.r.Store, key, func(state *assemblyState) error {
		if state.Revision == 0 {
			*state = assemblyState{Revision: 1, Pages: map[string]int{}, Counts: map[string]int64{}}
		}
		return nil
	}); err != nil {
		return false, err
	}
	state, _, err := Read[assemblyState](ctx, w.r.Store, key)
	if err != nil {
		return false, err
	}
	if state.Revision == 0 || state.Phase < 0 || state.Phase > 2 || state.Pages == nil || state.Counts == nil {
		return false, storage.ErrCorruptObject
	}
	w.Snapshot.Pages, w.Snapshot.Counts = maps.Clone(state.Pages), maps.Clone(state.Counts)
	w.Snapshot.UniqueManifests = state.UniqueManifests
	w.images, w.objects = map[string][]Image{}, nil
	if state.Phase == 2 {
		return true, nil
	}
	prefix := "captures/"
	if state.Phase == 1 {
		prefix = "cache-entries/"
	}
	entries, next, err := storage.Scan(ctx, w.r.Store, w.key(prefix), state.Token, limit)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if err := w.assembleEntry(ctx, entry.Key, state.Phase); err != nil {
			return false, err
		}
	}
	if err := w.Flush(ctx); err != nil {
		return false, err
	}
	updated := assemblyState{Revision: state.Revision + 1, Phase: state.Phase, Token: next, Pages: w.Snapshot.Pages, Counts: w.Snapshot.Counts, UniqueManifests: w.Snapshot.UniqueManifests}
	if next == "" {
		updated.Phase++
	}
	err = Update(ctx, w.r.Store, key, func(current *assemblyState) error {
		if reflect.DeepEqual(*current, updated) {
			return nil
		}
		if !reflect.DeepEqual(*current, state) {
			return storage.ErrConditionFailed
		}
		*current = updated
		return nil
	})
	return updated.Phase == 2, err
}

func (w *Writer) assembleEntry(ctx context.Context, key string, phase int) error {
	if phase == 1 {
		object, _, err := Read[Object](ctx, w.r.Store, key)
		if err != nil {
			return err
		}
		w.objects = append(w.objects, object)
		if len(w.objects) == PageSize {
			return w.flushObjects(ctx)
		}
		return nil
	}
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
	if err := w.validateLinks(ctx, image); err != nil {
		return err
	}
	w.images[image.Scope] = append(w.images[image.Scope], image)
	w.Snapshot.Counts[image.Scope+":"+image.Category]++
	if image.Scope == "platform" {
		w.Snapshot.UniqueManifests++
	}
	if len(w.images[image.Scope]) == PageSize {
		return w.flushImages(ctx, image.Scope)
	}
	return nil
}

func (w *Writer) requireNoPending(ctx context.Context) error {
	entries, next, err := storage.Scan(ctx, w.r.Store, w.key("pending/"), "", 1)
	if err != nil {
		return err
	}
	if len(entries) != 0 || next != "" {
		return ErrIncomplete
	}
	return nil
}

func (w *Writer) assembled(ctx context.Context) error {
	state, _, err := Read[assemblyState](ctx, w.r.Store, w.key("assembly.json"))
	if errors.Is(err, storage.ErrNotFound) {
		return ErrIncomplete
	}
	if err != nil {
		return err
	}
	if state.Phase != 2 {
		return ErrIncomplete
	}
	if err := w.requireNoPending(ctx); err != nil {
		return err
	}
	w.Snapshot.Pages, w.Snapshot.Counts = maps.Clone(state.Pages), maps.Clone(state.Counts)
	w.Snapshot.UniqueManifests = state.UniqueManifests
	return nil
}
