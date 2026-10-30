package inventory

import (
	"context"
	"registry-hub/internal/storage"
)

// Cursor retains the current page because storage continuation tokens are opaque.
type Cursor struct {
	Token    string          `json:"token,omitempty"`
	Next     string          `json:"next,omitempty"`
	Entries  []storage.Entry `json:"entries,omitempty"`
	Offset   int             `json:"offset,omitempty"`
	Done     bool            `json:"done,omitempty"`
	Entities uint64          `json:"entities,omitempty"`
}

func (c *Cursor) Peek(ctx context.Context, store storage.Store, prefix string, limit int) (storage.Entry, bool, error) {
	if c.Done {
		return storage.Entry{}, false, nil
	}
	if c.Offset < 0 || c.Offset > len(c.Entries) {
		return storage.Entry{}, false, storage.ErrCorruptObject
	}
	if c.Offset == len(c.Entries) {
		entries, next, err := storage.Scan(ctx, store, prefix, c.Token, limit)
		if err != nil {
			return storage.Entry{}, false, err
		}
		c.Entries, c.Next, c.Offset = entries, next, 0
		if len(entries) == 0 {
			c.Token, c.Done = next, next == ""
			return storage.Entry{}, false, nil
		}
	}
	return c.Entries[c.Offset], true, nil
}

func (c *Cursor) Advance() {
	c.Entities++
	c.Offset++
	if c.Offset == len(c.Entries) {
		c.Token, c.Done = c.Next, c.Next == ""
		c.Entries, c.Offset = nil, 0
	}
}
