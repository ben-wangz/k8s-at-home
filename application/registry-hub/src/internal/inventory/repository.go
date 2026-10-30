package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

var (
	ErrInvalid        = errors.New("invalid inventory query")
	ErrExpired        = errors.New("inventory snapshot or token expired")
	ErrCurrentExpired = errors.New("latest inventory snapshot expired")
	ErrNotObserved    = errors.Join(storage.ErrUnavailable, errors.New("inventory snapshot not observed"))
	ErrSuperseded     = errors.New("inventory builder was superseded")
	ErrIncomplete     = errors.New("inventory snapshot members are incomplete")
)

type Repository struct{ Store storage.Store }

func Read[T any](ctx context.Context, store storage.Store, key string) (T, string, error) {
	var value T
	object, err := store.Get(ctx, key)
	if err != nil {
		return value, "", err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 16<<20+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return value, "", err
	}
	if len(data) > 16<<20 {
		return value, "", storage.ErrCorruptObject
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, "", errors.Join(storage.ErrCorruptObject, err)
	}
	return value, object.Version, nil
}

func Update[T any](ctx context.Context, store storage.Store, key string, change func(*T) error) error {
	for attempt := 0; attempt < 16; attempt++ {
		value, version, err := Read[T](ctx, store, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		before, _ := json.Marshal(value)
		if err := change(&value); err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(data) > 16<<20 {
			return storage.ErrCorruptObject
		}
		if version != "" && bytes.Equal(before, data) {
			return nil
		}
		if guard, ok := ctx.Value(writeGuardKey{}).(func(context.Context) error); ok {
			if err := guard(ctx); err != nil {
				return err
			}
		}
		_, err = store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: version == "", IfMatch: version})
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrUnknownCommit) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			actual, _, readErr := Read[T](ctx, store, key)
			if readErr == nil {
				encoded, _ := json.Marshal(actual)
				if bytes.Equal(encoded, data) {
					return nil
				}
			} else if !errors.Is(readErr, storage.ErrNotFound) {
				return errors.Join(err, readErr)
			}
		}
	}
	return storage.ErrUnavailable
}

func (r *Repository) SaveContent(ctx context.Context, content Content) error {
	content.SchemaVersion = 1
	return Update(ctx, r.Store, Prefix+"contents/"+content.ContentID(content.Digest)+".json", func(old *Content) error {
		if old.SchemaVersion != 0 && (old.Generation > content.Generation || old.Generation == content.Generation && old.UpdatedAt.After(content.UpdatedAt)) {
			return nil
		}
		if old.Generation == content.Generation {
			if old.PlatformUnknown {
				content.PlatformUnknown = true
				content.Platform = nil
			}
			if old.Source == "manifest" && content.Source != "manifest" {
				return nil
			}
			if content.Platform == nil {
				content.Platform = old.Platform
			}
			if old.ClosureKnown && !content.ClosureKnown {
				return nil
			}
			if content.Subject == "" {
				content.Subject = old.Subject
			}
			if content.ArtifactType == "" {
				content.ArtifactType = old.ArtifactType
			}
		}
		*old = content
		return nil
	})
}

func (r *Repository) Content(ctx context.Context, identity Identity, digest string) (Content, error) {
	content, _, err := Read[Content](ctx, r.Store, Prefix+"contents/"+identity.ContentID(digest)+".json")
	if err == nil && (content.SchemaVersion != 1 || content.Authority != identity.Authority || content.Repository != identity.Repository || content.Digest != digest) {
		err = storage.ErrCorruptObject
	}
	return content, err
}

func (r *Repository) Bind(ctx context.Context, binding Binding) (Binding, error) {
	binding.SchemaVersion = 1
	err := Update(ctx, r.Store, Prefix+"bindings/"+binding.Identity.ID()+".json", func(old *Binding) error {
		if old.SchemaVersion != 0 && old.Generation == binding.Generation && old.Digest != binding.Digest && binding.Source == "distribution" && old.Source != "distribution" {
			binding = *old
			return nil
		}
		if old.SchemaVersion != 0 && (old.Generation > binding.Generation || old.Generation == binding.Generation && old.ConfirmedAt.After(binding.ConfirmedAt)) {
			binding = *old
			return nil
		}
		if old.Revision != "" && old.Digest == binding.Digest && old.Generation == binding.Generation && old.ContentGeneration == binding.ContentGeneration {
			binding.Revision = old.Revision
		} else {
			binding.Revision = ID("revision", binding.Identity.ID(), binding.Digest, binding.ConfirmedAt.Format(time.RFC3339Nano))
		}
		*old = binding
		return nil
	})
	return binding, err
}

func LifecycleVersion(imageID, revision string, generation uint64) string {
	data, _ := json.Marshal(generation)
	return ID("lifecycle", imageID, revision, string(data))
}

func (r *Repository) Lifecycle(ctx context.Context, version string) (Lifecycle, error) {
	value, _, err := Read[Lifecycle](ctx, r.Store, Prefix+"lifecycle/"+version+".json")
	if errors.Is(err, storage.ErrNotFound) {
		return Lifecycle{SchemaVersion: 1, Version: version}, nil
	}
	if err == nil && (value.SchemaVersion != 1 || value.Version != version) {
		err = storage.ErrCorruptObject
	}
	return value, err
}

func (r *Repository) Register(ctx context.Context, version string, knownNew bool) error {
	return Update(ctx, r.Store, Prefix+"lifecycle/"+version+".json", func(value *Lifecycle) error {
		if value.Version != "" {
			return nil
		}
		*value = Lifecycle{SchemaVersion: 1, Version: version}
		if knownNew {
			started := false
			value.Started = &started
		}
		return nil
	})
}

func (r *Repository) Start(ctx context.Context, version string) error {
	return r.StartAt(ctx, version, time.Now().UTC())
}

type writeGuardKey struct{}

func WithWriteGuard(ctx context.Context, guard func(context.Context) error) context.Context {
	if previous, ok := ctx.Value(writeGuardKey{}).(func(context.Context) error); ok {
		next := guard
		guard = func(ctx context.Context) error {
			if err := previous(ctx); err != nil {
				return err
			}
			return next(ctx)
		}
	}
	return context.WithValue(ctx, writeGuardKey{}, guard)
}

func (r *Repository) StartAt(ctx context.Context, version string, at time.Time) error {
	if at.IsZero() {
		return storage.ErrCorruptObject
	}
	return Update(ctx, r.Store, Prefix+"lifecycle/"+version+".json", func(value *Lifecycle) error {
		if value.Started != nil && *value.Started && value.FirstStartedAt != nil && !value.FirstStartedAt.After(at) {
			return nil
		}
		started := true
		*value = Lifecycle{SchemaVersion: 1, Version: version, Started: &started, FirstStartedAt: &at}
		return nil
	})
}
