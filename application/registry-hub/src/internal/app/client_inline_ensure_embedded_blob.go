package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
)

func (s *Service) ensureEmbeddedBlob(ctx context.Context, internalRepository string, descriptor artifact.Descriptor, beforeWrite func() error) error {
	blobPath := "/v2/" + internalRepository + "/blobs/" + descriptor.Digest
	exists, err := s.verifyEmbeddedBlob(ctx, blobPath, descriptor)
	if err != nil || exists {
		return err
	}
	if beforeWrite != nil {
		if err := beforeWrite(); err != nil {
			return err
		}
	}

	create, err := s.registry.Do(ctx, http.MethodPost, "/v2/"+internalRepository+"/blobs/uploads/", nil, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	if create.StatusCode != http.StatusAccepted {
		_ = create.Body.Close()
		return fmt.Errorf("Distribution upload creation returned status %d", create.StatusCode)
	}
	location := create.Header.Get("Location")
	_ = create.Body.Close()
	locationPath, query, err := s.registry.ParseLocation(location, internalRepository)
	if err != nil {
		s.abortInternalUpload(location, internalRepository)
		return err
	}
	query.Set("digest", descriptor.Digest)
	put, err := s.registry.Do(ctx, http.MethodPut, locationPath, query,
		http.Header{"Content-Type": []string{"application/octet-stream"}},
		bytes.NewReader(descriptor.Embedded), int64(len(descriptor.Embedded)))
	if err != nil {
		s.abortInternalUpload(location, internalRepository)
		return err
	}
	putDigest := put.Header.Get("Docker-Content-Digest")
	putStatus := put.StatusCode
	_ = put.Body.Close()
	if putStatus != http.StatusCreated || putDigest != descriptor.Digest {
		s.abortInternalUpload(location, internalRepository)
		return fmt.Errorf("Distribution inline blob commit returned status %d", putStatus)
	}
	exists, err = s.verifyEmbeddedBlob(ctx, blobPath, descriptor)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("Distribution inline blob was not readable after commit")
	}
	return nil
}

func (s *Service) ensureEmbeddedManifest(ctx context.Context, routed route.Route, manifest inlineManifest, beforeWrite func() error) error {
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(manifest.digest)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return err
	}
	exists, nativeSubjectHeader, err := verifyEmbeddedManifestResponse(response, manifest, s.cfg.Limits.ManifestBytes)
	if err != nil || exists {
		if err != nil {
			return err
		}
		return s.publishEmbeddedReferrer(ctx, routed, manifest, nativeSubjectHeader)
	}
	if beforeWrite != nil {
		if err := beforeWrite(); err != nil {
			return err
		}
	}
	put, err := s.registry.Do(ctx, http.MethodPut, path, nil,
		http.Header{"Content-Type": []string{manifest.mediaType}}, bytes.NewReader(manifest.data), int64(len(manifest.data)))
	if err != nil {
		return err
	}
	if put.StatusCode != http.StatusCreated || put.Header.Get("Docker-Content-Digest") != manifest.digest {
		status := put.StatusCode
		_ = put.Body.Close()
		return fmt.Errorf("Distribution inline manifest PUT returned status %d", status)
	}
	nativeSubjectHeader = put.Header.Get("OCI-Subject")
	_ = put.Body.Close()
	verify, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return err
	}
	verified, _, err := verifyEmbeddedManifestResponse(verify, manifest, s.cfg.Limits.ManifestBytes)
	if err != nil {
		return err
	}
	if !verified {
		return errors.New("Distribution inline manifest was not readable after commit")
	}
	return s.publishEmbeddedReferrer(ctx, routed, manifest, nativeSubjectHeader)
}

func verifyEmbeddedManifestResponse(response *http.Response, manifest inlineManifest, maxBytes int64) (bool, string, error) {
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodGet, route.EndpointManifest)
		_ = response.Body.Close()
		if !miss {
			return false, "", errors.New("Distribution inline manifest miss was not confirmed")
		}
		return false, "", nil
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return false, "", fmt.Errorf("Distribution inline manifest GET returned status %d", status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	nativeSubjectHeader := response.Header.Get("OCI-Subject")
	contentType := response.Header.Get("Content-Type")
	returnedDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if err != nil || int64(len(data)) > maxBytes || int64(len(data)) != int64(len(manifest.data)) || !bytes.Equal(data, manifest.data) || artifact.VerifyDigest(manifest.digest, data) != nil || !sameMediaType(contentType, manifest.mediaType) || returnedDigest != manifest.digest {
		return false, "", errors.New("existing Distribution manifest does not match inline descriptor")
	}
	return true, nativeSubjectHeader, nil
}

func (s *Service) publishEmbeddedReferrer(ctx context.Context, routed route.Route, manifest inlineManifest, nativeSubjectHeader string) error {
	if manifest.parsed.Subject == nil {
		return nil
	}
	_, err := s.publishReferrerManifest(ctx, routed, manifest.data, manifest.mediaType, manifest.digest, nativeSubjectHeader, true)
	return err
}

func (s *Service) verifyEmbeddedBlob(ctx context.Context, path string, descriptor artifact.Descriptor) (bool, error) {
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
		if response.ContentLength != descriptor.Size || response.Header.Get("Docker-Content-Digest") != descriptor.Digest {
			return false, errors.New("existing Distribution blob does not match inline descriptor")
		}
		return true, nil
	default:
		return false, fmt.Errorf("Distribution inline blob HEAD returned status %d", response.StatusCode)
	}
}

func releaseContentLeases(releases []func() error) {
	for index := len(releases) - 1; index >= 0; index-- {
		_ = releases[index]()
	}
}
