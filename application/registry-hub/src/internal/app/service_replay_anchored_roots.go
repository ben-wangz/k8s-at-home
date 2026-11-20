package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

const (
	recoveredBlobRootPageSize     = 100
	recoveredBlobRootScanLimit    = 300
	recoveredBlobRootAttemptLimit = 16
)

func (s *Service) replayAnchoredRootsForRecoveredBlob(ctx context.Context, authority, repository, digest string) error {
	if s.gate == nil || s.records == nil || s.tasks == nil || s.registry == nil {
		return nil
	}
	state := s.gate.State()
	if state.State != "open" || state.Exclusive || state.RecoveryBarrier {
		return nil
	}

	token := ""
	scanned, attempted := 0, 0
	for scanned < recoveredBlobRootScanLimit && attempted < recoveredBlobRootAttemptLimit {
		references, next, err := s.records.ListCacheProtectionReferences(ctx, digest, token, recoveredBlobRootPageSize)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, reference := range references {
			scanned++
			if reference.Authority != authority || reference.Repository != repository {
				continue
			}
			root, err := s.tasks.Get(ctx, reference.TaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if root.Kind != tasks.RootKind || root.Authority != authority || root.Repository != repository || root.RootDigest == "" || root.Reference == "" || root.CancelRequested || root.Status != tasks.Solidified || !root.RootPublished || !root.TerminalCleanupComplete {
				continue
			}
			attempted++
			eligible, err := s.anchoredRootGraphContainsRecoveredBlob(ctx, root, digest)
			if err != nil {
				return err
			}
			if !eligible {
				continue
			}
			current, err := s.generation(ctx, authority, repository, root.Reference)
			if err != nil {
				return err
			}
			if current.Tombstone || current.Generation != root.Generation {
				continue
			}
			matches, err := s.distributionReferenceMatchesRoot(ctx, root)
			if err != nil {
				return err
			}
			if !matches {
				continue
			}
			latest, err := s.generation(ctx, authority, repository, root.Reference)
			if err != nil {
				return err
			}
			if latest.Tombstone || latest.Generation != current.Generation {
				continue
			}
			_, _, err = s.tasks.EnqueueResolvedIfIdle(ctx, authority, repository, root.Reference, root.RootDigest, root.MediaType, root.Generation)
			if errors.Is(err, tasks.ErrActiveRootDemand) {
				continue
			}
			return err
		}
		if next == "" || scanned >= recoveredBlobRootScanLimit {
			return nil
		}
		token = next
	}
	return nil
}

func (s *Service) anchoredRootGraphContainsRecoveredBlob(ctx context.Context, root tasks.Task, digest string) (bool, error) {
	manifestCurrent, err := s.anchoredGraphNodeCurrent(ctx, root, tasks.GraphManifest, root.RootDigest)
	if err != nil {
		return false, err
	}
	if !manifestCurrent {
		return false, nil
	}
	blobCurrent, err := s.anchoredGraphNodeCurrent(ctx, root, tasks.GraphBlob, digest)
	if err != nil {
		return false, err
	}
	if !blobCurrent {
		return false, nil
	}
	scopeID, err := protectionScopeID(root.Authority, root.Repository, root.RootDigest, root.Generation)
	if err != nil {
		return false, err
	}
	session, err := s.gate.readProtectionSession(ctx, stagedSessionID(scopeID))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	terminalAnchored := session.State == "anchored" || session.State == "released" && session.TerminalReason == "anchored"
	return terminalAnchored && session.ScopeID == scopeID && session.PendingOperations == 0 && !session.UncertainRemoteResult && session.RemoteWrite == nil && session.RemoteWritePermit == "", nil
}

func (s *Service) anchoredGraphNodeCurrent(ctx context.Context, root tasks.Task, role tasks.GraphRole, digest string) (bool, error) {
	node, err := s.records.GetGraphNode(ctx, root.ID, role, digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if node.LocalState != "anchored" || !node.DiscoveryComplete || node.Generation != root.Generation || node.ContentGeneration == nil {
		return false, nil
	}
	current, err := s.generation(ctx, root.Authority, root.Repository, digest)
	if err != nil {
		return false, err
	}
	if current.Tombstone || current.Generation != *node.ContentGeneration {
		return false, nil
	}
	if _, err := s.records.FindLocalObservation(ctx, root.Authority, root.Repository, role, digest, node.ActualSize, current.Generation, node.GCEpoch); errors.Is(err, storage.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) distributionReferenceMatchesRoot(ctx context.Context, root tasks.Task) (bool, error) {
	internal, err := route.InternalName(root.Authority + "/" + root.Repository)
	if err != nil {
		return false, err
	}
	path := "/v2/" + internal + "/manifests/" + url.PathEscape(root.Reference)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, errors.New("Distribution root reference could not be verified")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(body)) > s.cfg.Limits.ManifestBytes || response.ContentLength >= 0 && int64(len(body)) != response.ContentLength || artifact.VerifyDigest(root.RootDigest, body) != nil {
		return false, nil
	}
	headerDigest := response.Header.Get("Docker-Content-Digest")
	return (headerDigest == "" || headerDigest == root.RootDigest) && sameMediaType(response.Header.Get("Content-Type"), root.MediaType), nil
}
