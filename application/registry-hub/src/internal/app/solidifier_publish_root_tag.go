package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"registry-hub/internal/artifact"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) publishRootTag(ctx context.Context, task *tasks.Task, taskLease lease.Lease) (result error) {
	protectionSessionID := task.ProtectionSessionID
	finishOperation, err := s.beginProtectedOperation(ctx, protectionSessionID)
	if err != nil {
		return err
	}
	defer s.finishProtectedOperation(finishOperation, &result)
	key := tagLeaseKey(task.Authority, task.Repository, task.Reference)
	owner := task.ID + "-tag"
	tagCtx, tagLease, releaseTag, err := acquireRenewedLease(ctx, s.tasks.Leases(), key, owner, 60*time.Second)
	if err != nil {
		return temporary("tag_busy", err)
	}
	defer releaseTag()
	root, ok := task.Graph[task.RootDigest]
	if !ok || root.Size < 0 {
		return permanent("task_state", errors.New("root manifest metadata is missing from the task graph"))
	}
	isCurrent, err := s.tasks.IsCurrentInFlight(tagCtx, *task)
	if err != nil {
		return temporary("task_state", err)
	}
	if !isCurrent {
		return permanent("superseded", errors.New("root task no longer owns the requested reference"))
	}
	if err := s.tasks.BeginRootPublish(tagCtx, task, taskLease); err != nil {
		return err
	}
	if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
		return temporary("tag_lease", err)
	}
	if err := s.service.checkTaskGeneration(tagCtx, task); err != nil {
		return permanent("generation", err)
	}
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/manifests/" + url.PathEscape(task.Reference)
	remoteWrite := protectionRemoteWrite{Kind: "root_tag", Path: path, Digest: task.RootDigest, Size: root.Size, MediaType: task.MediaType}
	current, err := s.registry.Do(tagCtx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	if current.StatusCode == http.StatusOK {
		digest := current.Header.Get("Docker-Content-Digest")
		_ = current.Body.Close()
		if digest == task.RootDigest {
			if err := s.verifyLocalObject(tagCtx, path, task.RootDigest, root.Size); err != nil {
				return err
			}
			if err := s.resolveRemoteWrite(tagCtx, protectionSessionID, remoteWrite); err != nil {
				return err
			}
			return s.tasks.MarkRootPublished(tagCtx, task, taskLease)
		}
		if err := s.tasks.AbortRootPublish(tagCtx, task, taskLease); err != nil {
			return err
		}
		return permanent("tag_conflict", errors.New("tag already points to a different digest"))
	}
	_ = current.Body.Close()
	if current.StatusCode != http.StatusNotFound {
		if err := s.tasks.AbortRootPublish(tagCtx, task, taskLease); err != nil {
			return err
		}
		return temporary("distribution", fmt.Errorf("Distribution tag GET returned status %d", current.StatusCode))
	}
	object, err := s.cache.GetRetained(tagCtx, task.Authority, task.Repository, task.RootDigest, task.MediaType)
	if err != nil {
		if abortErr := s.tasks.AbortRootPublish(tagCtx, task, taskLease); abortErr != nil {
			return abortErr
		}
		return temporary("storage", err)
	}
	defer object.Body.Close()
	if err := s.tasks.CheckActive(tagCtx, task, taskLease); err != nil {
		return err
	}
	if err := s.service.checkTaskGeneration(tagCtx, task); err != nil {
		return permanent("generation", err)
	}
	if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
		return temporary("tag_lease", err)
	}
	if err := s.tasks.Leases().Validate(tagCtx, tagLease); err != nil {
		return temporary("tag_lease", err)
	}
	if err := s.markRemoteWrite(tagCtx, protectionSessionID, remoteWrite); err != nil {
		return err
	}
	put, err := s.registry.Do(tagCtx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{task.MediaType}}, object.Body, object.Size)
	if err != nil {
		return temporary("distribution", err)
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != task.RootDigest {
		return temporary("distribution", errors.New("root tag publication was not confirmed"))
	}
	if protectionSessionID != "" {
		if err := s.gate.MarkProtectionContentObserved(tagCtx, protectionSessionID); err != nil {
			return temporary("protection_state", err)
		}
	}
	if err := s.verifyLocalObject(ctx, path, task.RootDigest, root.Size); err != nil {
		return err
	}
	if err := s.resolveRemoteWrite(ctx, protectionSessionID, remoteWrite); err != nil {
		return err
	}
	return s.tasks.MarkRootPublished(ctx, task, taskLease)
}

func (s *solidifier) verifyLocalObject(ctx context.Context, path, digest string, size int64) error {
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return temporary("distribution", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return temporary("distribution", fmt.Errorf("Distribution verification GET returned status %d", response.StatusCode))
	}
	if response.ContentLength >= 0 && response.ContentLength != size {
		return permanent("distribution_invalid", errors.New("Distribution object Content-Length does not match its expected size"))
	}
	if headerDigest := response.Header.Get("Docker-Content-Digest"); headerDigest != "" && headerDigest != digest {
		return permanent("distribution_invalid", errors.New("Distribution object digest header does not match its expected digest"))
	}
	hasher, err := artifact.Hasher(digest)
	if err != nil {
		return permanent("manifest_invalid", err)
	}
	count, err := io.Copy(hasher, io.LimitReader(response.Body, size+1))
	if err != nil || count != size || artifact.DigestFromHasher(digest, hasher) != digest {
		return permanent("distribution_invalid", errors.New("Distribution object failed digest or size verification"))
	}
	return nil
}

func (s *solidifier) checkpoint(ctx context.Context, task *tasks.Task, taskLease lease.Lease, reference contentRef) error {
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	entry := tasks.GraphEntry{
		Digest: reference.digest, MediaType: reference.mediaType, Size: reference.size,
		ArtifactType: reference.artifactType, Annotations: cloneAnnotations(reference.annotations),
		Platform: append(json.RawMessage(nil), reference.platform...), URLs: append([]string(nil), reference.urls...),
	}
	if task.Checkpoints[reference.digest] == "verified" {
		if existing, ok := task.Graph[reference.digest]; ok && reflect.DeepEqual(existing, entry) {
			return nil
		}
	}
	if task.Checkpoints == nil {
		task.Checkpoints = make(map[string]string)
	}
	if task.Graph == nil {
		task.Graph = make(map[string]tasks.GraphEntry)
	}
	task.Checkpoints[reference.digest] = "verified"
	task.Graph[reference.digest] = entry
	task.GraphVersion++
	if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
		return temporary("task_state", err)
	}
	return nil
}
