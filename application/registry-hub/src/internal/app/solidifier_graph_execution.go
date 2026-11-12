package app

import (
	"context"
	"errors"
	"maps"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

type graphDependencySignal struct {
	SchemaVersion int       `json:"schemaVersion"`
	TaskID        string    `json:"taskID"`
	RoundID       string    `json:"roundID"`
	Generation    uint64    `json:"generation"`
	Digest        string    `json:"digest"`
	Source        string    `json:"source"`
	ObservedAt    time.Time `json:"observedAt"`
}

type graphExecution struct {
	SchemaVersion     int               `json:"schemaVersion"`
	RoundID           string            `json:"roundID"`
	Generation        uint64            `json:"generation"`
	GCEpoch           uint64            `json:"gcEpoch"`
	Revision          string            `json:"revision"`
	QuotaWakeRevision uint64            `json:"quotaWakeRevision"`
	RetryCount        int               `json:"retryCount"`
	Ready             map[string]bool   `json:"ready"`
	Signals           map[string]string `json:"signals"`
}

type graphSelection struct {
	epoch    uint64
	affected map[string]bool
	ready    map[string]bool
	signals  map[string]string
}

func graphSignalPrefix(id string) string {
	return "solidification/graphs/" + id + "/dependency-signals/"
}
func graphExecutionKey(id string) string { return "solidification/graphs/" + id + "/execution.json" }

func (s *Service) markRootDependencyChanged(ctx context.Context, rootID string, intent tasks.BlobIntent) error {
	root, err := s.tasks.Get(ctx, rootID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if root.Kind != tasks.RootKind || root.CancelRequested || root.Status == tasks.Cancelled || root.Status == tasks.Failed || root.Status == tasks.Solidified {
		return nil
	}
	next := graphDependencySignal{SchemaVersion: 1, TaskID: root.ID, RoundID: root.RoundID, Generation: root.Generation, Digest: intent.Identity.Digest, Source: notificationSource(intent, ""), ObservedAt: intent.UpdatedAt}
	key := graphSignalPrefix(root.ID) + inventory.ID(next.Digest) + ".json"
	err = inventory.Update(ctx, s.store, key, func(current *graphDependencySignal) error {
		if current.RoundID == next.RoundID && current.Generation == next.Generation && (current.Source == next.Source || current.ObservedAt.After(next.ObservedAt)) {
			return nil
		}
		*current = next
		return nil
	})
	if errors.Is(err, storage.ErrCorruptObject) {
		return saveGraphRecord(ctx, s.store, key, next)
	}
	return err
}

func (s *solidifier) selectGraphWork(ctx context.Context, task *tasks.Task, graph *stagedGraph) (graphSelection, error) {
	selection := graphSelection{epoch: s.gate.State().Epoch, ready: make(map[string]bool), signals: make(map[string]string)}
	if graph.revision == "" {
		return selection, nil
	}
	previous, _, err := inventory.Read[graphExecution](ctx, s.store, graphExecutionKey(task.ID))
	if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
		return selection, err
	}
	// Dependency wakes also advance QuotaWakeRevision. A completed pass has no pending quota admission.
	compatible := err == nil && graph.reused && previous.SchemaVersion == 1 && previous.RoundID == task.RoundID && previous.Generation == task.Generation && previous.GCEpoch == selection.epoch && previous.Revision == graph.revision && previous.RetryCount == task.RetryCount
	affected := make(map[string]bool)
	changed := false
	for token := ""; ; {
		entries, next, err := storage.Scan(ctx, s.store, graphSignalPrefix(task.ID), token, 500)
		if err != nil {
			return selection, err
		}
		for _, entry := range entries {
			signal, version, err := inventory.Read[graphDependencySignal](ctx, s.store, entry.Key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if errors.Is(err, storage.ErrCorruptObject) {
				compatible = false
				continue
			}
			if err != nil {
				return selection, err
			}
			if signal.SchemaVersion != 1 || signal.TaskID != task.ID || signal.RoundID != task.RoundID || signal.Generation != task.Generation {
				compatible = false
				continue
			}
			selection.signals[signal.Digest] = version
			if previous.Signals[signal.Digest] != version {
				changed = true
				markAffectedManifests(graph.parents, affected, signal.Digest)
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if compatible && changed {
		selection.affected = affected
		selection.affected[task.RootDigest] = true
		selection.ready = maps.Clone(previous.Ready)
		if selection.ready == nil {
			selection.ready = make(map[string]bool)
		}
	}
	return selection, nil
}

func markAffectedManifests(parents map[string][]string, affected map[string]bool, digest string) {
	queue := []string{digest}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, parent := range parents[current] {
			if !affected[parent] {
				affected[parent] = true
				queue = append(queue, parent)
			}
		}
	}
}

func (selection graphSelection) skip(task *tasks.Task, manifest *stagedManifest) (bool, bool) {
	ready, known := selection.ready[manifest.ref.digest]
	if selection.affected == nil || selection.affected[manifest.ref.digest] || !known || rootTaskHasProtection(task, manifest.sessionID) {
		return false, false
	}
	return ready, true
}

func (s *solidifier) graphWriteContext(ctx context.Context, task *tasks.Task, holder lease.Lease) context.Context {
	return inventory.WithWriteGuard(ctx, func(ctx context.Context) error {
		if err := s.tasks.CheckActive(ctx, task, holder); err != nil {
			return err
		}
		return s.service.checkTaskGeneration(ctx, task)
	})
}

func (s *solidifier) saveGraphExecution(ctx context.Context, task *tasks.Task, holder lease.Lease, graph *stagedGraph, selection graphSelection) error {
	if graph.revision == "" {
		return nil
	}
	next := graphExecution{SchemaVersion: 1, RoundID: task.RoundID, Generation: task.Generation, GCEpoch: selection.epoch, Revision: graph.revision, QuotaWakeRevision: task.QuotaWakeRevision, RetryCount: task.RetryCount, Ready: selection.ready, Signals: selection.signals}
	return saveGraphRecord(s.graphWriteContext(ctx, task, holder), s.store, graphExecutionKey(task.ID), next)
}
