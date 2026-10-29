package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"registry-hub/internal/storage"
	"time"
)

const PageSize = 100

type Freshness struct {
	State      string `json:"state"`
	AgeSeconds int64  `json:"ageSeconds"`
}

type Snapshot struct {
	SchemaVersion   int              `json:"schemaVersion"`
	SnapshotID      string           `json:"snapshotID"`
	BuildStartedAt  time.Time        `json:"buildStartedAt"`
	ObservedAt      time.Time        `json:"observedAt"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	Coverage        string           `json:"coverage"`
	Building        bool             `json:"building"`
	Reasons         []string         `json:"reasons"`
	Freshness       Freshness        `json:"freshness"`
	BuilderFence    uint64           `json:"builderFence"`
	Pages           map[string]int   `json:"pages"`
	Counts          map[string]int64 `json:"counts"`
	UniqueManifests int64            `json:"uniqueManifests"`
	GCEpoch         uint64           `json:"gcEpoch"`
}

type ImagePage struct {
	Snapshot
	Items     []Image `json:"items"`
	NextToken string  `json:"nextToken"`
}

type ObjectPage struct {
	Snapshot
	Items     []Object `json:"items"`
	NextToken string   `json:"nextToken"`
}

type Detail struct {
	Snapshot
	Image Image `json:"image"`
}

type Writer struct {
	r        *Repository
	Snapshot Snapshot
	images   map[string][]Image
	objects  []Object
}

func (r *Repository) ResumeWriter(snapshot Snapshot) *Writer {
	snapshot.Pages = maps.Clone(snapshot.Pages)
	snapshot.Counts = maps.Clone(snapshot.Counts)
	return &Writer{r: r, Snapshot: snapshot, images: map[string][]Image{}}
}

func (w *Writer) Flush(ctx context.Context) error {
	for _, scope := range []string{"reference", "platform"} {
		if err := w.flushImages(ctx, scope); err != nil {
			return err
		}
	}
	return w.flushObjects(ctx)
}

func (r *Repository) NewWriter(now time.Time, ttl time.Duration, fence uint64) *Writer {
	return &Writer{r: r, Snapshot: Snapshot{SchemaVersion: 1, SnapshotID: ID("snapshot", now.Format(time.RFC3339Nano), fmt.Sprint(fence)), BuildStartedAt: now, ExpiresAt: now.Add(ttl), BuilderFence: fence, Pages: map[string]int{}, Counts: map[string]int64{}, Reasons: []string{}}, images: map[string][]Image{}}
}

func (w *Writer) key(suffix string) string {
	return Prefix + "snapshots/" + w.Snapshot.SnapshotID + "/" + suffix
}

func (r *Repository) Immutable(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("inventory snapshot page exceeds size limit")
	}
	_, err = r.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrUnknownCommit) && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	actual, _, readErr := Read[json.RawMessage](ctx, r.Store, key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if !bytes.Equal(actual, data) {
		return storage.ErrConditionFailed
	}
	return nil
}

func (w *Writer) AddImage(ctx context.Context, image Image, objects []Object, platforms []string) error {
	ready, err := w.HasImage(ctx, image.ID)
	if err != nil || ready {
		return err
	}
	if err := w.captureOpen(ctx); err != nil {
		return err
	}
	member := snapshotMember{ID: image.ID, Scope: image.Scope}
	if err := w.r.Immutable(ctx, w.key("captures/"+image.ID+".json"), member); err != nil {
		return err
	}
	if err := w.r.Immutable(ctx, w.key("entries/"+image.ID+".json"), image); err != nil {
		return err
	}
	if err := w.writeObjects(ctx, "objects/"+image.ID, objects); err != nil {
		return err
	}
	if platforms == nil {
		platforms = []string{}
	}
	for start := 0; start < len(platforms); start += PageSize {
		if err := w.r.Immutable(ctx, w.key(fmt.Sprintf("platforms/%s/%08d.json", image.ID, start/PageSize)), platforms[start:min(start+PageSize, len(platforms))]); err != nil {
			return err
		}
	}
	if err := w.r.Immutable(ctx, w.key("platforms/"+image.ID+"/count.json"), (len(platforms)+PageSize-1)/PageSize); err != nil {
		return err
	}
	return w.r.Immutable(ctx, w.key("members/"+image.ID+".json"), member)
}

func (w *Writer) writeObjects(ctx context.Context, prefix string, objects []Object) error {
	for start := 0; start < len(objects); start += PageSize {
		if err := w.r.Immutable(ctx, w.key(fmt.Sprintf("%s/%08d.json", prefix, start/PageSize)), objects[start:min(start+PageSize, len(objects))]); err != nil {
			return err
		}
	}
	return w.r.Immutable(ctx, w.key(prefix+"/count.json"), (len(objects)+PageSize-1)/PageSize)
}

func (w *Writer) AddCacheObject(ctx context.Context, object Object) error {
	_, _, err := Read[Object](ctx, w.r.Store, w.key("cache-entries/"+object.ID+".json"))
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := w.captureOpen(ctx); err != nil {
		return err
	}
	return w.r.Immutable(ctx, w.key("cache-entries/"+object.ID+".json"), object)
}

func (w *Writer) flushObjects(ctx context.Context) error {
	if len(w.objects) == 0 {
		return nil
	}
	if err := w.r.Immutable(ctx, w.key(fmt.Sprintf("cache/%08d.json", w.Snapshot.Pages["cache"])), w.objects); err != nil {
		return err
	}
	w.Snapshot.Pages["cache"]++
	w.objects = nil
	return nil
}

func (w *Writer) flushImages(ctx context.Context, scope string) error {
	if len(w.images[scope]) == 0 {
		return nil
	}
	if err := w.r.Immutable(ctx, w.key(fmt.Sprintf("%s/%08d.json", scope, w.Snapshot.Pages[scope])), w.images[scope]); err != nil {
		return err
	}
	w.Snapshot.Pages[scope]++
	w.images[scope] = nil
	return nil
}

func (w *Writer) Publish(ctx context.Context, complete bool, reasons []string) error {
	fence := w.Snapshot.BuilderFence
	if err := w.assembled(ctx); err != nil {
		return err
	}
	w.Snapshot.Coverage = "partial"
	if complete {
		w.Snapshot.Coverage = "complete"
	}
	w.Snapshot.Building = !complete
	w.Snapshot.Reasons = append(w.Snapshot.Reasons, reasons...)
	w.Snapshot.ObservedAt = w.Snapshot.BuildStartedAt
	w.Snapshot.ExpiresAt = time.Now().UTC().Add(w.Snapshot.ExpiresAt.Sub(w.Snapshot.BuildStartedAt))
	stored, _, readErr := Read[Snapshot](ctx, w.r.Store, w.key("manifest.json"))
	if readErr == nil {
		w.Snapshot = stored
	} else if !errors.Is(readErr, storage.ErrNotFound) {
		return readErr
	}
	if errors.Is(readErr, storage.ErrNotFound) {
		if err := w.r.Immutable(ctx, w.key("manifest.json"), w.Snapshot); err != nil {
			return err
		}
	}
	publication := w.Snapshot
	publication.BuilderFence = fence
	return Update(ctx, w.r.Store, Prefix+"current.json", func(current *Snapshot) error {
		if current.BuilderFence > fence || current.BuilderFence == fence && current.BuildStartedAt.After(w.Snapshot.BuildStartedAt) {
			return ErrSuperseded
		}
		*current = publication
		return nil
	})
}
