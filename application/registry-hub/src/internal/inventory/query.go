package inventory

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type Query struct {
	Scope      string
	ImageID    string
	Kind       string
	SnapshotID string
	Limit      int
	Token      string
	Filters    url.Values
}

type cursor struct {
	SnapshotID string    `json:"snapshotID"`
	Query      string    `json:"query"`
	Page       int       `json:"page"`
	Offset     int       `json:"offset"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

func (q Query) fingerprint() string {
	filters := url.Values{}
	for key, values := range q.Filters {
		if key != "token" && key != "limit" && key != "snapshotID" {
			filters[key] = values
		}
	}
	return ID("query", q.Kind, q.Scope, q.ImageID, filters.Encode())
}

func (r *Repository) prepare(ctx context.Context, q Query, key []byte, stale time.Duration) (Snapshot, cursor, error) {
	if q.Limit < 1 || q.Limit > 1000 {
		return Snapshot{}, cursor{}, ErrInvalid
	}
	c := cursor{Query: q.fingerprint(), SnapshotID: q.SnapshotID}
	if q.Token != "" {
		parts := strings.Split(q.Token, ".")
		if len(parts) != 2 || len(q.Token) > 4096 {
			return Snapshot{}, c, ErrInvalid
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(parts[0]))
		sig, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
			return Snapshot{}, c, ErrInvalid
		}
		data, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil || json.Unmarshal(data, &c) != nil {
			return Snapshot{}, c, ErrInvalid
		}
		if c.Query != q.fingerprint() || q.SnapshotID != "" && q.SnapshotID != c.SnapshotID || c.Page < 0 || c.Offset < 0 || c.Offset >= PageSize {
			return Snapshot{}, c, ErrInvalid
		}
		if !c.ExpiresAt.After(time.Now()) {
			return Snapshot{}, c, ErrExpired
		}
	}
	snapshot, err := r.Snapshot(ctx, c.SnapshotID, stale)
	if err != nil {
		return snapshot, c, err
	}
	c.SnapshotID = snapshot.SnapshotID
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = minTime(time.Now().Add(15*time.Minute), snapshot.ExpiresAt)
	}
	return snapshot, c, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func token(key []byte, c cursor) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func readPage[T any](ctx context.Context, r *Repository, snapshot Snapshot, kind string, page int) ([]T, error) {
	items, _, err := Read[[]T](ctx, r.Store, fmt.Sprintf("%ssnapshots/%s/%s/%08d.json", Prefix, snapshot.SnapshotID, kind, page))
	if errors.Is(err, storage.ErrNotFound) {
		err = storage.ErrCorruptObject
	}
	return items, err
}

func (r *Repository) Images(ctx context.Context, q Query, key []byte, stale time.Duration) (ImagePage, error) {
	snapshot, c, err := r.prepare(ctx, q, key, stale)
	result := ImagePage{Snapshot: snapshot, Items: []Image{}}
	if err != nil {
		return result, err
	}
	kind, pages := q.Scope, snapshot.Pages[q.Scope]
	if q.Kind == "platforms" {
		if _, err := r.Image(ctx, snapshot, q.ImageID); err != nil {
			return result, err
		}
		kind = "platforms/" + q.ImageID
		pages, _, err = Read[int](ctx, r.Store, Prefix+"snapshots/"+snapshot.SnapshotID+"/"+kind+"/count.json")
		if err != nil {
			return result, snapshotPageError(err)
		}
	}
	for c.Page < pages {
		var items []Image
		if q.Kind == "platforms" {
			ids, err := readPage[string](ctx, r, snapshot, kind, c.Page)
			if err != nil {
				return result, err
			}
			for _, id := range ids {
				image, err := r.Image(ctx, snapshot, id)
				if err != nil {
					return result, snapshotPageError(err)
				}
				items = append(items, image)
			}
		} else {
			items, err = readPage[Image](ctx, r, snapshot, kind, c.Page)
			if err != nil {
				return result, err
			}
		}
		for c.Offset < len(items) {
			item := items[c.Offset]
			c.Offset++
			matched, matchErr := r.matchesInventoryImage(ctx, snapshot, item, q.Filters)
			if matchErr != nil {
				return result, matchErr
			}
			if matched {
				result.Items = append(result.Items, item)
			}
			if len(result.Items) == q.Limit {
				break
			}
		}
		if c.Offset >= len(items) {
			c.Page++
			c.Offset = 0
		}
		if len(result.Items) == q.Limit {
			break
		}
	}
	if c.Page < pages {
		result.NextToken, err = token(key, c)
	}
	return result, err
}

func (r *Repository) Image(ctx context.Context, snapshot Snapshot, id string) (Image, error) {
	if !ValidID(id) {
		return Image{}, ErrInvalid
	}
	image, _, err := Read[Image](ctx, r.Store, Prefix+"snapshots/"+snapshot.SnapshotID+"/entries/"+id+".json")
	return image, err
}

func (r *Repository) Objects(ctx context.Context, q Query, key []byte, stale time.Duration) (ObjectPage, error) {
	snapshot, c, err := r.prepare(ctx, q, key, stale)
	result := ObjectPage{Snapshot: snapshot, Items: []Object{}}
	if err != nil {
		return result, err
	}
	kind, pages := "cache", snapshot.Pages["cache"]
	if q.ImageID != "" {
		if _, err := r.Image(ctx, snapshot, q.ImageID); err != nil {
			return result, err
		}
		kind = "objects/" + q.ImageID
		pages, _, err = Read[int](ctx, r.Store, Prefix+"snapshots/"+snapshot.SnapshotID+"/"+kind+"/count.json")
		if err != nil {
			return result, snapshotPageError(err)
		}
	}
	for c.Page < pages {
		items, err := readPage[Object](ctx, r, snapshot, kind, c.Page)
		if err != nil {
			return result, err
		}
		for c.Offset < len(items) {
			item := items[c.Offset]
			c.Offset++
			if matchesObject(item, q.Filters) {
				result.Items = append(result.Items, item)
			}
			if len(result.Items) == q.Limit {
				break
			}
		}
		if c.Offset >= len(items) {
			c.Page++
			c.Offset = 0
		}
		if len(result.Items) == q.Limit {
			break
		}
	}
	if c.Page < pages {
		result.NextToken, err = token(key, c)
	}
	return result, err
}
