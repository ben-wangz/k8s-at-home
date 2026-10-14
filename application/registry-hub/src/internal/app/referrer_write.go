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
	"strings"
	"time"
)

type referrerManifestInfo struct {
	ArtifactType string               `json:"artifactType"`
	Annotations  map[string]string    `json:"annotations"`
	Config       *artifact.Descriptor `json:"config"`
	Subject      *artifact.Descriptor `json:"subject"`
}

type referrerFallbackMarker struct {
	SchemaVersion int    `json:"schemaVersion"`
	Authority     string `json:"authority"`
	Repository    string `json:"repository"`
	Subject       string `json:"subject"`
	Tag           string `json:"tag"`
	Digest        string `json:"digest"`
}

var ErrNativeSubjectHeader = errors.New("Distribution native referrers response omitted the expected OCI-Subject header")

func (s *Service) publishReferrerManifest(ctx context.Context, routed route.Route, data []byte, mediaType, digest, nativeSubjectHeader string, requireSubjectHeader bool, taskIDs ...string) (string, error) {
	parsed, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return "", invalidReferrer("referrer manifest failed validation")
	}
	if parsed.Subject == nil {
		return "", nil
	}
	if !route.ValidDigest(parsed.Subject.Digest) || artifact.VerifyDigest(digest, data) != nil {
		return "", invalidReferrer("referrer manifest identity is invalid")
	}
	descriptor, err := buildReferrerDescriptor(data, mediaType, digest, parsed)
	if err != nil {
		return "", err
	}
	native, err := s.nativeReferrersAvailable(ctx, routed, parsed.Subject.Digest)
	if err != nil {
		return "", err
	}
	migrationPending, err := s.referrerMigrationPending(ctx, routed, parsed.Subject.Digest, native)
	if err != nil {
		return "", err
	}
	if native {
		if requireSubjectHeader && nativeSubjectHeader != parsed.Subject.Digest {
			return "", ErrNativeSubjectHeader
		}
		if migrationPending {
			if err := s.updateFallbackReferrers(ctx, routed, parsed.Subject.Digest, descriptor, false, taskIDs...); err != nil {
				return "", err
			}
		}
		return parsed.Subject.Digest, nil
	}
	if err := s.updateFallbackReferrers(ctx, routed, parsed.Subject.Digest, descriptor, false, taskIDs...); err != nil {
		return "", err
	}
	return parsed.Subject.Digest, nil
}

func buildReferrerDescriptor(data []byte, mediaType, digest string, manifest artifact.Manifest) (json.RawMessage, error) {
	var info referrerManifestInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Subject == nil {
		return nil, invalidReferrer("referrer manifest metadata is invalid")
	}
	descriptor := map[string]any{
		"mediaType": mediaType,
		"digest":    digest,
		"size":      len(data),
	}
	artifactType := info.ArtifactType
	if artifactType == "" && mediaType != referrerIndexMediaType && info.Config != nil {
		artifactType = info.Config.MediaType
	}
	if artifactType != "" {
		descriptor["artifactType"] = artifactType
	}
	if len(info.Annotations) > 0 {
		descriptor["annotations"] = info.Annotations
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	if err := artifact.VerifyDigest(digest, data); err != nil || manifest.Subject.Digest != info.Subject.Digest {
		return nil, invalidReferrer("referrer descriptor does not match the manifest")
	}
	return encoded, nil
}

func (s *Service) nativeReferrersAvailable(ctx context.Context, routed route.Route, subject string) (bool, error) {
	authority, repository := routeStorageIdentity(routed)
	cacheKey := authority + "\x00" + repository
	s.nativeMu.Lock()
	if capability, ok := s.nativeCache[cacheKey]; ok && time.Now().Before(capability.expiresAt) {
		s.nativeMu.Unlock()
		return capability.available, nil
	}
	delete(s.nativeCache, cacheKey)
	s.nativeMu.Unlock()
	path := "/v2/" + routed.Internal + "/referrers/" + url.PathEscape(subject)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		s.invalidateNativeReferrers(cacheKey)
		return false, err
	}
	if response.StatusCode == http.StatusOK {
		_, err := s.readReferrerIndex(response)
		if err != nil {
			s.invalidateNativeReferrers(cacheKey)
			return false, err
		}
		s.cacheNativeReferrers(cacheKey, true)
		return true, nil
	}
	if nativeReferrerMiss(response) {
		_ = response.Body.Close()
		if err := s.confirmRepositoryExists(ctx, routed); err != nil {
			s.invalidateNativeReferrers(cacheKey)
			return false, err
		}
		s.cacheNativeReferrers(cacheKey, false)
		return false, nil
	}
	status := response.StatusCode
	_ = response.Body.Close()
	s.invalidateNativeReferrers(cacheKey)
	return false, fmt.Errorf("internal referrers probe returned status %d", status)
}

func (s *Service) confirmRepositoryExists(ctx context.Context, routed route.Route) error {
	query := url.Values{"n": []string{"1"}}
	response, err := s.registry.Do(ctx, http.MethodGet, "/v2/"+routed.Internal+"/tags/list", query, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	status := response.StatusCode
	_ = response.Body.Close()
	if status != http.StatusOK {
		return fmt.Errorf("could not confirm repository existence for native Referrers capability: status %d", status)
	}
	return nil
}

func (s *Service) cacheNativeReferrers(key string, available bool) {
	s.nativeMu.Lock()
	if s.nativeCache == nil {
		s.nativeCache = make(map[string]nativeReferrerCapability)
	}
	s.nativeCache[key] = nativeReferrerCapability{available: available, expiresAt: time.Now().Add(30 * time.Second)}
	s.nativeMu.Unlock()
}

func (s *Service) invalidateNativeReferrers(key string) {
	s.nativeMu.Lock()
	delete(s.nativeCache, key)
	s.nativeMu.Unlock()
}

func (s *Service) removeReferrerBeforeDelete(ctx context.Context, routed route.Route, reference string) (*referrerRepairIntent, error) {
	if !strings.Contains(reference, ":") {
		return nil, nil
	}
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(reference)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{referrerIndexMediaType, manifestAccept}}, nil, 0)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
		_ = response.Body.Close()
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return nil, registryStatusError("manifest lookup before delete", status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		_ = response.Body.Close()
		return nil, invalidReferrer("manifest Content-Type before delete is invalid")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	manifestDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if readErr != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes {
		return nil, invalidReferrer("manifest body before delete is invalid or too large")
	}
	if manifestDigest == "" {
		manifestDigest = artifact.CalculateSHA256(data)
	}
	if strings.Contains(reference, ":") && manifestDigest != reference || artifact.VerifyDigest(manifestDigest, data) != nil {
		return nil, invalidReferrer("manifest digest before delete does not match its reference")
	}
	parsed, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return nil, invalidReferrer("manifest before delete failed validation")
	}
	if parsed.Subject == nil {
		return nil, nil
	}
	native, err := s.nativeReferrersAvailable(ctx, routed, parsed.Subject.Digest)
	if err != nil {
		return nil, err
	}
	migrationPending, err := s.referrerMigrationPending(ctx, routed, parsed.Subject.Digest, native)
	if err != nil {
		return nil, err
	}
	if native && !migrationPending {
		return nil, nil
	}
	descriptor, err := buildReferrerDescriptor(data, mediaType, manifestDigest, parsed)
	if err != nil {
		return nil, invalidReferrer("manifest descriptor before delete is invalid")
	}
	return s.updateFallbackReferrersForDelete(ctx, routed, parsed.Subject.Digest, descriptor)
}

func (s *Service) updateFallbackReferrers(ctx context.Context, routed route.Route, subject string, descriptor json.RawMessage, remove bool, taskIDs ...string) error {
	var taskID string
	if len(taskIDs) > 0 {
		taskID = taskIDs[0]
	}
	_, err := s.mutateFallbackReferrers(ctx, routed, subject, descriptor, remove, false, taskID)
	return err
}

func (s *Service) updateFallbackReferrersForDelete(ctx context.Context, routed route.Route, subject string, descriptor json.RawMessage) (*referrerRepairIntent, error) {
	return s.mutateFallbackReferrers(ctx, routed, subject, descriptor, true, true, "")
}
