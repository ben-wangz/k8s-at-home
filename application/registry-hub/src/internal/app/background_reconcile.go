package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strings"
	"sync"
	"time"
)

type completedDownloadSource struct {
	Authority  string `json:"authority"`
	Repository string `json:"repository"`
	Identity   string `json:"identity"`
	Digest     string `json:"digest"`
	TotalSize  int64  `json:"totalSize"`
	Offset     int64  `json:"offset"`
}

func (s *Service) runBackgroundReconciliation(ctx context.Context) {
	var group sync.WaitGroup
	for _, source := range []struct {
		kind, prefix string
		step         func(context.Context, string) error
	}{
		{"source_tasks", "tasks/state/", s.reconcileInventoryTask},
		{"source_downloads", "downloads/v1/", s.reconcileCommittedDownload},
		{"source_cache", "cache/objects/", s.reconcileManifestCache},
		{"source_demands", "solidification/blobs/owners/", s.reconcileBlobDemandWork},
	} {
		group.Add(1)
		go func() {
			defer group.Done()
			s.runSourceSweep(ctx, source.kind, source.prefix, func(ctx context.Context, entry storage.Entry) error { return source.step(ctx, entry.Key) })
		}()
	}
	group.Wait()
}

func (s *Service) reconcileBlobDemandWork(ctx context.Context, key string) error {
	demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, key)
	if err != nil || demand.RootTaskID == "" {
		return err
	}
	intent, err := s.records.GetBlobIntent(ctx, demand.IntentID)
	if err != nil {
		return err
	}
	q := workqueue.Queue{Store: s.store}
	if intent.State != tasks.IntentWaitingCache && intent.State != tasks.IntentWaitingQuota {
		source := inventory.ID("demand-wake", intent.ID, demand.DemandID, string(intent.State), intent.CacheVersion, demand.CreatedAt.String())
		if _, err := q.Enqueue(ctx, "wake_root", source, demand.RootTaskID, demand.CreatedAt); err != nil {
			return err
		}
	}
	_, _, err = s.demandExecutionWork(ctx, demand)
	return err
}

func (s *Service) reconcileInventoryTask(ctx context.Context, key string) error {
	task, _, err := inventory.Read[tasks.Task](ctx, s.store, key)
	if err != nil {
		return err
	}
	if task.RootDigest == "" || task.ManifestObservedAt.IsZero() {
		return nil
	}
	return s.deferManifestProjection(ctx, inventoryManifestWork{Identity: inventoryIdentity(task.Authority, task.Repository, task.Reference), Digest: task.RootDigest, MediaType: task.MediaType, Generation: task.Generation, ContentGeneration: task.ManifestGeneration, KnownNew: !s.inventoryInstalledAt.IsZero() && task.CreatedAt.After(s.inventoryInstalledAt), Source: "upstream"}, task.ManifestObservedAt)
}

func (s *Service) reconcileCommittedDownload(ctx context.Context, key string) error {
	if !strings.HasSuffix(key, "/state.json") {
		return nil
	}
	value, _, err := inventory.Read[completedDownloadSource](ctx, s.store, key)
	if err != nil {
		return err
	}
	if value.Authority == "" || value.Repository == "" || value.TotalSize < 0 || value.TotalSize != value.Offset {
		return nil
	}
	cached, err := s.cache.GetAnyRetained(ctx, value.Authority, value.Repository, value.Digest)
	if err != nil {
		return err
	}
	_ = cached.Body.Close()
	return s.deferDownloadCleanup(ctx, value.Authority, value.Repository, value.Identity, value.Digest, 0)
}

func (s *Service) reconcileManifestCache(ctx context.Context, key string) error {
	if !strings.Contains(key, "/representations/") || !strings.HasSuffix(key, ".json") {
		return nil
	}
	metadata, _, err := inventory.Read[struct {
		Authority        string    `json:"authority"`
		Repository       string    `json:"repository"`
		Digest           string    `json:"digest"`
		MediaType        string    `json:"mediaType"`
		VerifiedAt       time.Time `json:"verifiedAt"`
		InventoryTracked bool      `json:"inventoryTracked"`
	}](ctx, s.store, key)
	if err != nil {
		return err
	}
	inspection, err := s.cache.Inspect(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaType)
	if err != nil {
		return err
	}
	if !inspection.Present || metadata.VerifiedAt.IsZero() {
		return nil
	}
	generation, err := s.generation(ctx, metadata.Authority, metadata.Repository, metadata.Digest)
	if err != nil || generation.Tombstone || metadata.VerifiedAt.Before(generation.UpdatedAt) {
		return err
	}
	if !inventoryManifestType(metadata.MediaType) {
		return s.processCachedBlobIntent(ctx, tasks.BlobIdentity{Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest})
	}
	return s.deferManifestProjection(ctx, inventoryManifestWork{Identity: inventoryIdentity(metadata.Authority, metadata.Repository, metadata.Digest), Digest: metadata.Digest, MediaType: metadata.MediaType, Generation: generation.Generation, ContentGeneration: generation.Generation, KnownNew: metadata.InventoryTracked, Source: "cache"}, metadata.VerifiedAt)
}
