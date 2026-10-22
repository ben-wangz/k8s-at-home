package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
)

func (s *Service) recoverProtectionRemoteOperations(ctx context.Context) error {
	after := ""
	for {
		sessions, next, err := s.gate.ListProtectionSessions(ctx, after, 100)
		if err != nil {
			return err
		}
		for _, item := range sessions {
			session, active, err := s.gate.protectionSession(ctx, item.ID)
			if err != nil {
				return err
			}
			if !active || session.PendingOperations == 0 && !session.UncertainRemoteResult {
				continue
			}

			var verifiedWrite *protectionRemoteWrite
			if session.UncertainRemoteResult {
				if session.RemoteWrite == nil {
					return s.blockUnknownProtectionWrite(ctx, item.ID, errors.New("remote write identity was not persisted"))
				}
				verified, verifyErr := s.verifyProtectionRemoteWrite(ctx, *session.RemoteWrite)
				if verifyErr != nil {
					return s.blockUnknownProtectionWrite(ctx, item.ID, verifyErr)
				}
				if !verified {
					return s.blockUnknownProtectionWrite(ctx, item.ID, errors.New("remote write is not visible yet"))
				}
				if err := s.gate.MarkRecoveredProtectionContentObserved(ctx, item.ID); err != nil {
					return err
				}
				write := *session.RemoteWrite
				verifiedWrite = &write
			}
			if err := s.gate.CompleteProtectionStartupRecovery(ctx, item.ID, verifiedWrite); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		after = next
	}
}

func (s *Service) blockUnknownProtectionWrite(ctx context.Context, sessionID string, cause error) error {
	if err := s.gate.SetProtectionRecoveryRequired(ctx, sessionID, "remote_result_unknown"); err != nil {
		return errors.Join(ErrRecoveryBarrier, cause, err)
	}
	return fmt.Errorf("%w: session %s: %v", ErrRecoveryBarrier, sessionID, cause)
}

func (s *Service) verifyProtectionRemoteWrite(ctx context.Context, write protectionRemoteWrite) (bool, error) {
	switch write.Kind {
	case "blob":
		response, err := s.registry.Do(ctx, http.MethodHead, write.Path, nil, make(http.Header), nil, 0)
		if err != nil {
			return false, err
		}
		if response.StatusCode == http.StatusNotFound {
			miss := confirmedMiss(response, http.MethodHead, route.EndpointBlob)
			_ = response.Body.Close()
			if miss {
				return false, nil
			}
			return false, errors.New("Distribution returned an untrusted blob miss during recovery")
		}
		if response.StatusCode != http.StatusOK {
			status := response.StatusCode
			_ = response.Body.Close()
			return false, fmt.Errorf("Distribution blob HEAD returned status %d during recovery", status)
		}
		valid := response.ContentLength == write.Size && response.Header.Get("Docker-Content-Digest") == write.Digest
		_ = response.Body.Close()
		if !valid {
			return false, errors.New("Distribution blob identity does not match the interrupted write")
		}
		if err := (&solidifier{registry: s.registry}).verifyLocalObject(ctx, write.Path, write.Digest, write.Size); err != nil {
			return false, err
		}
		return true, nil
	case "manifest", "root_tag":
		return s.verifyProtectionManifestWrite(ctx, write)
	case "referrer":
		return s.verifyProtectionReferrerWrite(ctx, write)
	default:
		return false, errors.New("remote write kind is unsupported during recovery")
	}
}

func (s *Service) verifyProtectionManifestWrite(ctx context.Context, write protectionRemoteWrite) (bool, error) {
	response, err := s.registry.Do(ctx, http.MethodGet, write.Path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodGet, route.EndpointManifest)
		_ = response.Body.Close()
		if miss {
			return false, nil
		}
		return false, errors.New("Distribution returned an untrusted manifest miss during recovery")
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return false, fmt.Errorf("Distribution manifest GET returned status %d during recovery", status)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	contentType := response.Header.Get("Content-Type")
	headerDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if readErr != nil || int64(len(data)) != write.Size || int64(len(data)) > s.cfg.Limits.ManifestBytes || artifact.VerifyDigest(write.Digest, data) != nil {
		return false, errors.Join(errors.New("Distribution manifest failed digest or size verification during recovery"), readErr)
	}
	if headerDigest != "" && headerDigest != write.Digest {
		return false, errors.New("Distribution manifest digest header does not match the interrupted write")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !sameMediaType(mediaType, write.MediaType) {
		return false, errors.New("Distribution manifest media type does not match the interrupted write")
	}
	return true, nil
}

func (s *Service) verifyProtectionReferrerWrite(ctx context.Context, write protectionRemoteWrite) (bool, error) {
	response, err := s.registry.Do(ctx, http.MethodGet, write.Path, nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusOK {
		index, readErr := s.collectReferrerPages(response, write.Path, "", func(query url.Values) (*http.Response, error) {
			return s.registry.Do(ctx, http.MethodGet, write.Path, query, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
		})
		if readErr != nil {
			return false, readErr
		}
		if referrerIndexContains(index, write.Digest) {
			return true, nil
		}
	} else if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodGet, route.EndpointReferrers)
		_ = response.Body.Close()
		if !miss {
			return false, errors.New("Distribution returned an untrusted referrers miss during recovery")
		}
	} else {
		status := response.StatusCode
		_ = response.Body.Close()
		return false, fmt.Errorf("Distribution referrers GET returned status %d during recovery", status)
	}

	fallback, err := s.registry.Do(ctx, http.MethodGet, write.FallbackPath, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return false, err
	}
	if fallback.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(fallback, http.MethodGet, route.EndpointManifest)
		_ = fallback.Body.Close()
		if miss {
			return false, nil
		}
		return false, errors.New("Distribution returned an untrusted fallback referrers miss during recovery")
	}
	if fallback.StatusCode != http.StatusOK {
		status := fallback.StatusCode
		_ = fallback.Body.Close()
		return false, fmt.Errorf("Distribution fallback referrers GET returned status %d during recovery", status)
	}
	index, err := s.readReferrerIndex(fallback)
	if err != nil {
		return false, err
	}
	return referrerIndexContains(index, write.Digest), nil
}

func referrerIndexContains(index referrerIndex, digest string) bool {
	for _, raw := range index.manifests {
		var descriptor referrerDescriptor
		if json.Unmarshal(raw, &descriptor) == nil && descriptor.Digest == digest {
			return true
		}
	}
	return false
}
