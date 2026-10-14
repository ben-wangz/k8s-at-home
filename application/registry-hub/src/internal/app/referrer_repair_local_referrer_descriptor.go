package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

func (s *Service) localReferrerDescriptor(ctx context.Context, routed route.Route, digest, subject string) (json.RawMessage, error) {
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(digest)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return nil, registryStatusError("referrer manifest GET", status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		_ = response.Body.Close()
		return nil, invalidReferrer("referrer manifest media type is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	_ = response.Body.Close()
	if err != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes || response.Header.Get("Docker-Content-Digest") != digest && artifact.CalculateSHA256(data) != digest {
		return nil, invalidReferrer("referrer manifest digest or size is invalid")
	}
	if artifact.VerifyDigest(digest, data) != nil {
		return nil, invalidReferrer("referrer manifest digest is invalid")
	}
	parsed, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil || parsed.Subject == nil || parsed.Subject.Digest != subject {
		return nil, invalidReferrer("referrer manifest subject does not match its repair intent")
	}
	return buildReferrerDescriptor(data, mediaType, digest, parsed)
}

func (s *Service) publishLocalReferrerToNative(ctx context.Context, routed route.Route, intent *referrerRepairIntent) error {
	descriptor, err := s.localReferrerDescriptor(ctx, routed, intent.Referrer, intent.Subject)
	if err != nil {
		return err
	}
	var value struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(descriptor, &value) != nil || value.MediaType == "" {
		return invalidReferrer("referrer media type is missing")
	}
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(intent.Referrer)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		_ = response.Body.Close()
		return registryStatusError("referrer manifest GET before native publish", status)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	returnedDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if mediaErr != nil || mediaType != value.MediaType || err != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes || returnedDigest != intent.Referrer || artifact.VerifyDigest(intent.Referrer, data) != nil {
		return invalidReferrer("referrer manifest body is invalid")
	}
	put, err := s.registry.Do(ctx, http.MethodPut, path, nil, http.Header{"Content-Type": []string{value.MediaType}}, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	status, digest, subject := put.StatusCode, put.Header.Get("Docker-Content-Digest"), put.Header.Get("OCI-Subject")
	_ = put.Body.Close()
	if status != http.StatusCreated || digest != intent.Referrer {
		return registryStatusError("native referrer manifest PUT", status)
	}
	if subject != intent.Subject {
		return ErrNativeSubjectHeader
	}
	return nil
}

func (s *Service) deleteLocalReferrerManifest(ctx context.Context, routed route.Route, digest string) error {
	path := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(digest)
	response, err := s.registry.Do(ctx, http.MethodDelete, path, nil, nil, nil, 0)
	if err != nil {
		return err
	}
	status := response.StatusCode
	if status == http.StatusNotFound && confirmedMiss(response, http.MethodDelete, route.EndpointManifest) {
		_ = response.Body.Close()
		return nil
	}
	_ = response.Body.Close()
	if status != http.StatusAccepted && status != http.StatusNotFound {
		return registryStatusError("referrer manifest DELETE", status)
	}
	verify, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		return err
	}
	confirmed := verify.StatusCode == http.StatusNotFound && confirmedMiss(verify, http.MethodGet, route.EndpointManifest)
	_ = verify.Body.Close()
	if !confirmed {
		return errors.New("referrer manifest delete could not be verified")
	}
	return nil
}

func (s *Service) retryReferrerIntent(ctx context.Context, intent *referrerRepairIntent) error {
	intent.Retryable = true
	intent.NextAttemptAt = time.Now().UTC()
	intent.ErrorClass = ""
	intent.UpdatedAt = time.Now().UTC()
	return s.updateReferrerRepair(ctx, intent)
}

func decodeReferrerRepair(ctx context.Context, store storage.Store, entry storage.Entry) (*referrerRepairIntent, error) {
	object, err := store.Get(ctx, entry.Key)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("referrer repair state exceeds size limit")
	}
	var intent referrerRepairIntent
	if json.Unmarshal(data, &intent) != nil || !validReferrerRepair(intent) {
		return nil, errors.New("referrer repair state is invalid")
	}
	intent.version = object.Version
	return &intent, nil
}

func referrerIntentPage(ctx context.Context, store storage.Store, routed route.Route, subject, last string, limit int) ([]referrerRepairIntent, string, error) {
	authority, repository := routeStorageIdentity(routed)
	token := ""
	items := make([]referrerRepairIntent, 0)
	for {
		entries, next, err := store.List(ctx, referrerRepairSubjectPrefix(authority, repository, subject), token, 500)
		if err != nil {
			return nil, "", err
		}
		for _, entry := range entries {
			intent, err := decodeReferrerRepair(ctx, store, entry)
			if err != nil {
				return nil, "", err
			}
			if intent.Authority == authority && intent.Repository == repository && intent.Subject == subject && intent.Referrer > last {
				items = append(items, *intent)
				if len(items) > 4096 {
					return nil, "", errors.New("referrer repair list exceeds its size limit")
				}
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(items) == 0 {
		return items, "", nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Referrer < items[j].Referrer })
	if len(items) <= limit {
		return items, "", nil
	}
	return items[:limit], items[limit-1].Referrer, nil
}
