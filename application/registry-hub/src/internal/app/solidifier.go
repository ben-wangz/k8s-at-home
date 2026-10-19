package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"strings"
	"time"
)

type solidifier struct {
	service  *Service
	cfg      config.Config
	cache    *artifact.Cache
	store    storage.Store
	registry *distribution.Client
	upstream *upstream.Client
	tasks    *tasks.Manager
	gate     *writeGate
}

type contentRef struct {
	digest       string
	mediaType    string
	size         int64
	artifactType string
	annotations  map[string]string
	platform     json.RawMessage
	urls         []string
}

type closure struct {
	blobs     []contentRef
	manifests []contentRef
	seen      map[string]contentRef
	active    map[string]bool
	objects   int
	bytes     int64
}

const (
	manifestAccept           = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	objectLeaseTTL           = 60 * time.Second
	objectLeaseRenewInterval = 20 * time.Second
)

func manifestReadHeaders() http.Header {
	return http.Header{"Accept": []string{manifestAccept}}
}

func (s *solidifier) Process(ctx context.Context, task *tasks.Task, taskLease lease.Lease) (result error) {
	defer func() {
		if result == nil || s.service == nil || s.service.logger == nil || errors.Is(result, context.Canceled) {
			return
		}
		class := "solidification"
		var failure *tasks.Failure
		if errors.As(result, &failure) {
			class = failure.Class
		}
		if errors.Is(result, lease.ErrLost) {
			class = "fencing"
		} else if errors.Is(result, artifact.ErrDigestMismatch) {
			class = "digest_mismatch"
		}
		if errors.Is(result, artifact.ErrDigestMismatch) {
			s.service.metrics.recordEvent("digest_mismatch")
		}
		if errors.Is(result, lease.ErrLost) || errors.Is(result, storage.ErrLeaseLost) {
			s.service.metrics.recordEvent("lease_lost")
		}
		s.service.logger.Warn("solidification failed", "task_id", task.ID, "error_class", class)
	}()
	releaseSlot, ok := s.service.acquireSolidificationSlot(ctx, false)
	if !ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.service.metrics.recordRejection("solidification_concurrency")
		return temporary("solidification_busy", errors.New("solidification worker slot is unavailable"))
	}
	defer releaseSlot()
	workCtx := ctx
	graph := &closure{seen: make(map[string]contentRef), active: make(map[string]bool)}
	root := contentRef{digest: task.RootDigest, mediaType: task.MediaType, size: -1}
	if entry, ok := task.Graph[task.RootDigest]; ok {
		root.size = entry.Size
	}
	if _, err := s.visitManifest(workCtx, task, taskLease, graph, root, 0, true); err != nil {
		return err
	}
	for _, blob := range graph.blobs {
		if err := s.ensureBlob(workCtx, task, taskLease, blob); err != nil {
			return err
		}
	}
	for _, manifest := range graph.manifests {
		if err := s.ensureManifest(workCtx, task, taskLease, manifest, manifest.digest); err != nil {
			return err
		}
	}
	if !strings.Contains(task.Reference, ":") {
		requestCtx, cancel := context.WithTimeout(workCtx, s.cfg.Limits.RequestTimeout)
		err := s.publishRootTag(requestCtx, task, taskLease)
		cancel()
		if err != nil {
			return err
		}
	}
	digests := make([]string, 0, len(graph.seen))
	for digest := range graph.seen {
		digests = append(digests, digest)
	}
	requestCtx, cancel := context.WithTimeout(workCtx, s.cfg.Limits.RequestTimeout)
	err := s.cache.MarkCleanupPending(requestCtx, task.Authority, task.Repository, digests, time.Now().UTC().Add(time.Hour))
	cancel()
	if err != nil {
		return temporary("storage", err)
	}
	return nil
}

func (s *solidifier) visitManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, graph *closure, reference contentRef, depth int, root bool) (contentRef, error) {
	if depth > s.cfg.Limits.ClosureDepth {
		return contentRef{}, permanent("closure_limit", errors.New("manifest recursion depth exceeds limit"))
	}
	if !root && reference.size <= 0 {
		return contentRef{}, permanent("manifest_invalid", errors.New("manifest descriptor size must be positive"))
	}
	if graph.active[reference.digest] {
		return contentRef{}, permanent("manifest_cycle", errors.New("manifest graph contains a cycle"))
	}
	if existing, ok := graph.seen[reference.digest]; ok {
		if reference.size > 0 && existing.size != reference.size {
			return contentRef{}, permanent("manifest_invalid", errors.New("descriptor sizes disagree for the same digest"))
		}
		if reference.mediaType != "" && existing.mediaType != reference.mediaType {
			return contentRef{}, permanent("manifest_invalid", errors.New("descriptor media types disagree for the same digest"))
		}
		return existing, nil
	}
	if err := artifactDigest(reference.digest); err != nil {
		return contentRef{}, permanent("manifest_invalid", err)
	}
	data, actual, err := s.readManifest(ctx, task.Authority, task.Repository, reference)
	if errors.Is(err, storage.ErrNotFound) {
		if fetchErr := s.fetchObject(ctx, task, taskLease, reference, true); fetchErr != nil {
			return contentRef{}, fetchErr
		}
		data, actual, err = s.readManifest(ctx, task.Authority, task.Repository, reference)
	}
	if err != nil {
		return contentRef{}, err
	}
	if !root && int64(len(data)) != reference.size {
		return contentRef{}, permanent("manifest_invalid", errors.New("manifest descriptor size mismatch"))
	}
	if reference.mediaType != "" && !sameMediaType(reference.mediaType, actual.mediaType) {
		return contentRef{}, permanent("manifest_invalid", errors.New("manifest descriptor media type mismatch"))
	}
	actual.size = int64(len(data))
	actual.artifactType = reference.artifactType
	actual.annotations = cloneAnnotations(reference.annotations)
	actual.platform = append(json.RawMessage(nil), reference.platform...)
	actual.urls = append([]string(nil), reference.urls...)
	graph.active[reference.digest] = true
	defer delete(graph.active, reference.digest)
	parsed, err := artifact.ParseManifest(data, actual.mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return contentRef{}, permanent("manifest_invalid", err)
	}
	if err := graph.addObject(actual, s.cfg.Limits); err != nil {
		return contentRef{}, err
	}
	graph.seen[reference.digest] = actual
	for _, descriptor := range append(copyDescriptors(parsed.Config), parsed.Layers...) {
		if err := s.visitBlob(ctx, task, taskLease, graph, descriptor); err != nil {
			return contentRef{}, err
		}
	}
	if parsed.Manifests != nil {
		for _, descriptor := range *parsed.Manifests {
			child := descriptorRef(descriptor)
			if len(descriptor.Embedded) > 0 || hasEmbeddedData(descriptor.Data) {
				if err := s.cachePut(ctx, task, taskLease, descriptor.Digest, descriptor.MediaType, bytes.NewReader(descriptor.Embedded), int64(len(descriptor.Embedded))); err != nil {
					return contentRef{}, temporary("storage", err)
				}
			}
			if _, err := s.visitManifest(ctx, task, taskLease, graph, child, depth+1, false); err != nil {
				return contentRef{}, err
			}
		}
	}
	if parsed.Subject != nil && hasEmbeddedData(parsed.Subject.Data) {
		if err := s.cachePut(ctx, task, taskLease, parsed.Subject.Digest, parsed.Subject.MediaType, bytes.NewReader(parsed.Subject.Embedded), int64(len(parsed.Subject.Embedded))); err != nil {
			return contentRef{}, temporary("storage", err)
		}
	}
	graph.manifests = append(graph.manifests, actual)
	if err := s.checkpoint(ctx, task, taskLease, actual); err != nil {
		return contentRef{}, err
	}
	return actual, nil
}

func (s *solidifier) visitBlob(ctx context.Context, task *tasks.Task, taskLease lease.Lease, graph *closure, descriptor artifact.Descriptor) error {
	if descriptor.Size > s.cfg.Limits.ObjectBytes {
		return permanent("object_limit", errors.New("descriptor size exceeds object limit"))
	}
	ref := descriptorRef(descriptor)
	if existing, ok := graph.seen[ref.digest]; ok {
		if existing.size != ref.size || existing.mediaType != "" && ref.mediaType != "" && !sameMediaType(existing.mediaType, ref.mediaType) {
			return permanent("manifest_invalid", errors.New("descriptor sizes disagree for the same digest"))
		}
		return nil
	}
	if hasEmbeddedData(descriptor.Data) {
		if err := s.cachePut(ctx, task, taskLease, ref.digest, ref.mediaType, bytes.NewReader(descriptor.Embedded), ref.size); err != nil {
			return temporary("storage", err)
		}
	} else if err := s.fetchObject(ctx, task, taskLease, ref, false); err != nil {
		return err
	}
	if hasEmbeddedData(descriptor.Data) && looksLikeManifest(descriptor.Embedded) {
		if _, err := s.visitManifest(ctx, task, taskLease, graph, ref, 1, false); err != nil {
			return err
		}
		graph.blobs = append(graph.blobs, ref)
		return s.checkpoint(ctx, task, taskLease, ref)
	}
	if err := graph.addObject(ref, s.cfg.Limits); err != nil {
		return err
	}
	graph.seen[ref.digest] = ref
	graph.blobs = append(graph.blobs, ref)
	return s.checkpoint(ctx, task, taskLease, ref)
}
