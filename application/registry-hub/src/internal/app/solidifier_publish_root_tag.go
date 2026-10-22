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
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"time"
)

func (s *solidifier) publishRootTag(ctx context.Context, task *tasks.Task, taskLease lease.Lease) (result error) {
	finishOperation, err := s.beginProtectedOperation(ctx, task)
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
			if err := s.resolveRemoteWrite(tagCtx, task, remoteWrite); err != nil {
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
	if err := s.markRemoteWrite(tagCtx, task, remoteWrite); err != nil {
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
	if task.ProtectionSessionID != "" {
		if err := s.gate.MarkProtectionContentObserved(tagCtx, taskProtectionSession(task)); err != nil {
			return temporary("protection_state", err)
		}
	}
	if err := s.verifyLocalObject(ctx, path, task.RootDigest, root.Size); err != nil {
		return err
	}
	if err := s.resolveRemoteWrite(ctx, task, remoteWrite); err != nil {
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

func (s *solidifier) cacheHas(ctx context.Context, task *tasks.Task, reference contentRef) (bool, error) {
	object, err := s.cache.GetRetained(ctx, task.Authority, task.Repository, reference.digest, reference.mediaType)
	if err == nil {
		_ = object.Body.Close()
		if reference.size >= 0 && object.Size != reference.size {
			return false, errors.New("cached object size differs from its descriptor")
		}
		return true, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if reference.mediaType != "" {
		return false, nil
	}
	object, err = s.cache.GetAnyRetained(ctx, task.Authority, task.Repository, reference.digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = object.Body.Close()
	if reference.size >= 0 && object.Size != reference.size {
		return false, errors.New("cached object size differs from its descriptor")
	}
	return true, nil
}

func (s *solidifier) retryUpstream(ctx context.Context, authority, repository, requestPath string, query url.Values, headers http.Header) (*http.Response, error) {
	var lastErr error
	waited := time.Duration(0)
	for attempt := 0; attempt < maxUpstreamRetryAttempts; attempt++ {
		var retryAfter string
		response, err := s.upstream.Do(ctx, authority, repository, http.MethodGet, requestPath, query, headers)
		if err != nil {
			lastErr = err
			if !upstream.Retryable(err) || attempt == maxUpstreamRetryAttempts-1 {
				return nil, lastErr
			}
		} else if response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusServiceUnavailable && response.StatusCode != http.StatusGatewayTimeout {
			return response, nil
		} else {
			retryAfter = response.Header.Get("Retry-After")
			lastErr = fmt.Errorf("upstream returned status %d", response.StatusCode)
			_ = response.Body.Close()
			if attempt == maxUpstreamRetryAttempts-1 {
				return nil, lastErr
			}
		}
		delay, ok := upstreamRetryDelay(retryAfter, attempt, waited)
		if !ok {
			return nil, lastErr
		}
		if _, hasRetryAfter := parseRetryAfter(retryAfter); !hasRetryAfter {
			waited += delay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func renewLease(ctx context.Context, cancel context.CancelFunc, manager *lease.Manager, current lease.Lease, done chan<- error) {
	renewLeaseWithInterval(ctx, cancel, manager, current, objectLeaseRenewInterval, done)
}
