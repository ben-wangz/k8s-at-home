package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func (s *Service) projectBlobExecutionScope(ctx context.Context, job *workqueue.Job, root tasks.Task, p inventoryExecutionProgress) (any, bool, error) {
	entry, ok, err := p.Nodes.Peek(ctx, s.store, "solidification/graphs/"+root.ID+"/nodes/", 32)
	if err != nil {
		return p, false, err
	}
	if ok {
		node, _, err := inventory.Read[tasks.GraphNode](ctx, s.store, entry.Key)
		if err != nil {
			return p, false, err
		}
		if node.Role == tasks.GraphManifest {
			scope, err := protectionScopeID(root.Authority, root.Repository, node.Digest, root.Generation)
			if err != nil {
				return p, false, err
			}
			if scope == p.Demand.ScopeID && node.ContentGeneration != nil {
				fact := inventoryExecutionFact{TaskID: root.ID, Identity: inventoryIdentity(root.Authority, root.Repository, root.Reference), RootDigest: root.RootDigest, Digest: node.Digest, Generation: root.Generation, ContentGeneration: *node.ContentGeneration}
				at := job.CreatedAt
				if p.Demand.CreatedAt.After(at) {
					at = p.Demand.CreatedAt
				}
				if err := s.projectExecutionFact(ctx, fact, at, 2); err != nil {
					return p, false, err
				}
				p.Nodes.Done = true
			}
		}
		p.Nodes.Advance()
	}
	if p.Nodes.Done {
		p.Demand, p.Nodes, p.Phase = nil, inventory.Cursor{}, 0
		var fact inventoryExecutionFact
		if json.Unmarshal(job.Payload, &fact) != nil {
			return p, false, storage.ErrCorruptObject
		}
		if fact.Demand != nil {
			p.Cursor.Done = true
		} else {
			p.Cursor.Advance()
		}
	}
	return p, false, nil
}
