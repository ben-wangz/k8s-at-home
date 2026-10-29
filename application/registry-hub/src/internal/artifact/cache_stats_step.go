package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type StatsCursor struct {
	Cursor   inventory.Cursor
	Stats    CacheStats
	Identity string
	Modified int64
	Data     *storage.Entry
	Complete map[string]bool
	Metadata []cacheMetadata
}

// StatsStep keeps only one identity in memory and publishes nothing before the sweep ends.
func (c *Cache) StatsStep(ctx context.Context, p *StatsCursor, orphanAfter time.Duration) (bool, error) {
	entry, ok, err := p.Cursor.Peek(ctx, c.store, "cache/objects/", 32)
	if err != nil {
		return false, err
	}
	if ok {
		identity, valid := cacheEntryIdentity(entry.Key)
		if valid {
			if p.Identity != "" && p.Identity != identity {
				p.finishIdentity(orphanAfter)
			}
			p.Identity = identity
			p.Modified = max(p.Modified, entry.ModTime)
			switch {
			case strings.HasSuffix(entry.Key, "/data"):
				p.Data = &entry
			case strings.Contains(entry.Key, "/complete/"):
				if len(p.Complete) >= cacheRepresentationIndexMaxMediaTypes {
					return false, storage.ErrCorruptObject
				}
				if p.Complete == nil {
					p.Complete = map[string]bool{}
				}
				p.Complete[entry.Key] = true
			case strings.Contains(entry.Key, "/representations/") && strings.HasSuffix(entry.Key, ".json"):
				metadata, _, err := c.readMetadataEntry(ctx, entry.Key)
				if errors.Is(err, storage.ErrNotFound) {
					break
				}
				if err != nil {
					return false, err
				}
				if !validCacheMetadataKey(entry.Key, metadata) || len(p.Metadata) >= cacheRepresentationIndexMaxMediaTypes {
					return false, storage.ErrCorruptObject
				}
				p.Metadata = append(p.Metadata, metadata)
			}
		}
		p.Cursor.Advance()
	}
	if p.Cursor.Done {
		p.finishIdentity(orphanAfter)
	}
	return p.Cursor.Done, nil
}

func (p *StatsCursor) finishIdentity(orphanAfter time.Duration) {
	if p.Identity == "" {
		return
	}
	committed, cleanup := false, false
	for _, metadata := range p.Metadata {
		if p.Complete[cacheCompleteKey(p.Identity, metadata.MediaType)] && (metadata.State == "complete" || metadata.State == "cleanup_pending") {
			committed, cleanup = true, cleanup || metadata.State == "cleanup_pending"
		}
	}
	if committed && p.Data != nil {
		p.Stats.Objects++
		p.Stats.Bytes += p.Data.Size
		if cleanup {
			p.Stats.CleanupPending++
		}
	} else if !committed {
		if p.Modified != 0 && p.Modified <= time.Now().Add(-orphanAfter).UnixNano() {
			p.Stats.Orphans++
		} else {
			p.Stats.Staging++
		}
	}
	p.Identity, p.Modified, p.Data, p.Complete, p.Metadata = "", 0, nil, nil, nil
}
