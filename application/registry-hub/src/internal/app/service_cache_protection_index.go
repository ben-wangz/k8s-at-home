package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
	"strings"
)

func recoveryEntryID(entry storage.Entry, prefix string) (string, error) {
	id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, prefix), ".json")
	if !validGateID(id) || entry.Key != prefix+id+".json" {
		return "", storage.ErrCorruptObject
	}
	return id, nil
}

func (r *protectionRecovery) rootDemandStep(ctx context.Context) error {
	c := &r.checkpoint
	if c.Entity == "" {
		entry, ok, err := c.Cursor.Peek(ctx, r.service.store, "solidification/blobs/intents/", 16)
		if err != nil {
			return err
		}
		if !ok {
			if c.Cursor.Done {
				r.next("cache_protection_index")
			}
			return nil
		}
		id, err := recoveryEntryID(entry, "solidification/blobs/intents/")
		if err != nil {
			return err
		}
		if _, err := r.service.records.GetBlobIntent(ctx, id); err != nil {
			return err
		}
		c.Entity, c.Inner = id, inventory.Cursor{}
		return nil
	}
	entry, ok, err := c.Inner.Peek(ctx, r.service.store, "solidification/blobs/owners/"+c.Entity+"/", 16)
	if err != nil {
		return err
	}
	if !ok {
		if c.Inner.Done {
			c.Cursor.Advance()
			c.Entity = ""
		}
		return nil
	}
	demand, err := r.service.records.ReadRecoveryBlobDemand(ctx, entry.Key, c.Entity)
	if err != nil {
		return err
	}
	if err := r.service.records.RepairRootBlobDemandIndex(ctx, demand); err != nil {
		return err
	}
	c.Inner.Advance()
	return nil
}

func (r *protectionRecovery) recoveryTask(ctx context.Context) (tasks.Task, bool, error) {
	c := &r.checkpoint
	if c.Entity == "" {
		entry, ok, err := c.Cursor.Peek(ctx, r.service.store, "tasks/state/", 16)
		if err != nil || !ok {
			return tasks.Task{}, false, err
		}
		c.Entity, err = recoveryEntryID(entry, "tasks/state/")
		if err != nil {
			return tasks.Task{}, false, err
		}
	}
	if c.Phase == "cache_protection_index" && r.indexTask != nil && r.indexTask.ID == c.Entity {
		return *r.indexTask, true, nil
	}
	task, err := r.service.tasks.Get(ctx, c.Entity)
	if err == nil && c.Phase == "cache_protection_index" {
		r.indexTask, r.indexDigests = &task, taskRecoveryDigests(task)
	}
	return task, err == nil, err
}

func taskRecoveryDigests(task tasks.Task) []string {
	seen := map[string]bool{}
	if task.RootDigest != "" {
		seen[task.RootDigest] = true
	}
	for key, entry := range task.Graph {
		if key != "" {
			seen[key] = true
		}
		if entry.Digest != "" {
			seen[entry.Digest] = true
		}
	}
	digests := make([]string, 0, len(seen))
	for digest := range seen {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests
}

func (r *protectionRecovery) cacheReferenceStep(ctx context.Context) error {
	c := &r.checkpoint
	task, ok, err := r.recoveryTask(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if c.Cursor.Done {
			r.next("legacy_owners")
		}
		return nil
	}
	if task.Kind != tasks.RootKind {
		c.Cursor.Advance()
		c.Entity = ""
		return nil
	}
	if c.Digest != "" {
		if err := r.service.records.RepairCacheProtectionHalf(ctx, task.ID, task.Authority, task.Repository, c.Digest, c.Half); err != nil {
			return err
		}
		c.Half++
		if c.Half == 2 {
			c.Digest, c.Half = "", 0
			if c.Step == "graph" {
				c.Inner.Advance()
			} else {
				c.Index++
			}
		}
		return nil
	}
	digests := r.indexDigests
	if c.Step != "graph" && c.Index < len(digests) {
		c.Digest = digests[c.Index]
		return nil
	}
	c.Step = "graph"
	entry, ok, err := c.Inner.Peek(ctx, r.service.store, "solidification/graphs/"+task.ID+"/nodes/", 16)
	if err != nil {
		return err
	}
	if ok {
		node, _, err := inventory.Read[tasks.GraphNode](ctx, r.service.store, entry.Key)
		if err != nil {
			return err
		}
		if node.RootTaskID != task.ID {
			return storage.ErrCorruptObject
		}
		verified, err := r.service.records.GetGraphNode(ctx, task.ID, node.Role, node.Digest)
		if err != nil {
			return err
		}
		if index := sort.SearchStrings(digests, verified.Digest); index < len(digests) && digests[index] == verified.Digest {
			c.Inner.Advance()
			return nil
		}
		c.Digest = verified.Digest
		return nil
	}
	if c.Inner.Done {
		c.Cursor.Advance()
		c.Entity, c.Step, c.Index, c.Inner = "", "", 0, inventory.Cursor{}
	}
	return nil
}
