package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

type referrerMigrationRecord struct {
	SchemaVersion       int       `json:"schemaVersion"`
	State               string    `json:"state"`
	Authority           string    `json:"authority"`
	Repository          string    `json:"repository"`
	Subject             string    `json:"subject"`
	Generation          uint64    `json:"generation"`
	FallbackIndexDigest string    `json:"fallbackIndexDigest,omitempty"`
	NativeIndexDigest   string    `json:"nativeIndexDigest"`
	Referrers           []string  `json:"referrers"`
	UpdatedAt           time.Time `json:"updatedAt"`
	version             string
}

const (
	referrerMigrationMigrating = "migrating"
	referrerMigrationComplete  = "complete"
)

type migrationReferrerDescriptor struct {
	Digest       string            `json:"digest"`
	MediaType    string            `json:"mediaType"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType"`
	Annotations  map[string]string `json:"annotations"`
}

func (s *Service) migrateFallbackReferrers(ctx context.Context, routed route.Route, subject string) (referrerMigrationRecord, error) {
	native, err := s.nativeReferrersAvailable(ctx, routed, subject)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	if !native {
		return referrerMigrationRecord{}, errors.New("native Referrers API is unavailable")
	}
	releaseGate, err := s.gate.BeginExclusive(ctx, 5*time.Minute)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	defer releaseGate()

	authority, repository := routeStorageIdentity(routed)
	leaseCtx, subjectLease, releaseSubject, err := s.acquireReferrerLease(ctx, routed, subject)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	defer releaseSubject()
	tag := referrerFallbackTag(subject)
	tagCtx, tagLease, releaseTag, err := acquireRenewedLease(leaseCtx, s.tasks.Leases(), tagLeaseKey(authority, repository, tag), requestID(), 60*time.Second)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	defer releaseTag()
	if err := validateMigrationLeases(leaseCtx, s.tasks.Leases(), subjectLease, tagCtx, tagLease); err != nil {
		return referrerMigrationRecord{}, err
	}

	fallback, err := s.readFallbackIndex(tagCtx, routed, subject, tag)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	digests := make([]string, 0, len(fallback.manifests))
	for _, raw := range fallback.manifests {
		var descriptor migrationReferrerDescriptor
		if json.Unmarshal(raw, &descriptor) != nil || !route.ValidDigest(descriptor.Digest) || descriptor.MediaType == "" || descriptor.Size < 1 {
			return referrerMigrationRecord{}, errors.New("fallback referrer descriptor is invalid")
		}
		actualData, err := s.localReferrerDescriptor(tagCtx, routed, descriptor.Digest, subject)
		if err != nil {
			return referrerMigrationRecord{}, err
		}
		var actual migrationReferrerDescriptor
		if json.Unmarshal(actualData, &actual) != nil || descriptor.Digest != actual.Digest || descriptor.MediaType != actual.MediaType || descriptor.Size != actual.Size || descriptor.ArtifactType != actual.ArtifactType || !equalStringMaps(descriptor.Annotations, actual.Annotations) {
			return referrerMigrationRecord{}, errors.New("fallback referrer descriptor does not match its manifest")
		}
		digests = append(digests, descriptor.Digest)
	}
	sort.Strings(digests)
	previous, previousErr := s.loadReferrerMigration(tagCtx, authority, repository, subject)
	if previousErr != nil && !errors.Is(previousErr, storage.ErrNotFound) {
		return referrerMigrationRecord{}, previousErr
	}
	if previousErr == nil && previous.State == referrerMigrationComplete && previous.FallbackIndexDigest == fallback.digest && equalStrings(previous.Referrers, digests) {
		nativeIndex, err := s.fetchCompleteLocalReferrers(tagCtx, routed, subject)
		if err != nil {
			return referrerMigrationRecord{}, err
		}
		if containsReferrerDigests(nativeIndex, digests) {
			return s.storeReferrerMigration(tagCtx, authority, repository, subject, fallback.digest, nativeIndex.digest, digests)
		}
	}
	if _, err := s.storePendingReferrerMigration(tagCtx, authority, repository, subject, fallback.digest, digests); err != nil {
		return referrerMigrationRecord{}, err
	}

	for _, digest := range digests {
		if err := validateMigrationLeases(leaseCtx, s.tasks.Leases(), subjectLease, tagCtx, tagLease); err != nil {
			return referrerMigrationRecord{}, err
		}
		intent := &referrerRepairIntent{Authority: authority, Repository: repository, Subject: subject, Referrer: digest}
		if err := s.publishLocalReferrerToNative(tagCtx, routed, intent); err != nil {
			return referrerMigrationRecord{}, err
		}
	}
	if err := validateMigrationLeases(leaseCtx, s.tasks.Leases(), subjectLease, tagCtx, tagLease); err != nil {
		return referrerMigrationRecord{}, err
	}
	nativeIndex, err := s.fetchCompleteLocalReferrers(tagCtx, routed, subject)
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	if !containsReferrerDigests(nativeIndex, digests) {
		return referrerMigrationRecord{}, errors.New("native Referrers API did not include every fallback referrer")
	}
	if err := validateMigrationLeases(leaseCtx, s.tasks.Leases(), subjectLease, tagCtx, tagLease); err != nil {
		return referrerMigrationRecord{}, err
	}
	return s.storeReferrerMigration(tagCtx, authority, repository, subject, fallback.digest, nativeIndex.digest, digests)
}

func containsReferrerDigests(index referrerIndex, digests []string) bool {
	nativeDigests := make(map[string]struct{}, len(index.manifests))
	for _, raw := range index.manifests {
		var descriptor referrerDescriptor
		if json.Unmarshal(raw, &descriptor) != nil || !route.ValidDigest(descriptor.Digest) {
			return false
		}
		nativeDigests[descriptor.Digest] = struct{}{}
	}
	for _, digest := range digests {
		if _, found := nativeDigests[digest]; !found {
			return false
		}
	}
	return true
}

func validateMigrationLeases(subjectCtx context.Context, manager *lease.Manager, subject lease.Lease, tagCtx context.Context, tag lease.Lease) error {
	if err := manager.Validate(subjectCtx, subject); err != nil {
		return err
	}
	return manager.Validate(tagCtx, tag)
}

func (s *Service) storeReferrerMigration(ctx context.Context, authority, repository, subject, fallbackDigest, nativeDigest string, digests []string) (referrerMigrationRecord, error) {
	return s.storeReferrerMigrationState(ctx, authority, repository, subject, referrerMigrationComplete, fallbackDigest, nativeDigest, digests)
}

func (s *Service) storePendingReferrerMigration(ctx context.Context, authority, repository, subject, fallbackDigest string, digests []string) (referrerMigrationRecord, error) {
	return s.storeReferrerMigrationState(ctx, authority, repository, subject, referrerMigrationMigrating, fallbackDigest, "", digests)
}

func (s *Service) storeReferrerMigrationState(ctx context.Context, authority, repository, subject, state, fallbackDigest, nativeDigest string, digests []string) (referrerMigrationRecord, error) {
	key := referrerMigrationKey(authority, repository, subject)
	for attempt := 0; attempt < 8; attempt++ {
		current, err := s.loadReferrerMigration(ctx, authority, repository, subject)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return referrerMigrationRecord{}, err
		}
		generation := uint64(1)
		options := storage.PutOptions{IfAbsent: errors.Is(err, storage.ErrNotFound)}
		if err == nil {
			if current.State == state && current.FallbackIndexDigest == fallbackDigest && current.NativeIndexDigest == nativeDigest && equalStrings(current.Referrers, digests) {
				return current, nil
			}
			generation = current.Generation + 1
			options.IfMatch = current.version
		}
		record := referrerMigrationRecord{
			SchemaVersion: 1, State: state, Authority: authority, Repository: repository, Subject: subject,
			Generation: generation, FallbackIndexDigest: fallbackDigest,
			NativeIndexDigest: nativeDigest, Referrers: append([]string(nil), digests...), UpdatedAt: time.Now().UTC(),
		}
		data, err := json.Marshal(record)
		if err != nil {
			return referrerMigrationRecord{}, err
		}
		entry, err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return referrerMigrationRecord{}, err
		}
		record.version = entry.Version
		return record, nil
	}
	return referrerMigrationRecord{}, storage.ErrConditionFailed
}

func (s *Service) loadReferrerMigration(ctx context.Context, authority, repository, subject string) (referrerMigrationRecord, error) {
	object, err := s.store.Get(ctx, referrerMigrationKey(authority, repository, subject))
	if err != nil {
		return referrerMigrationRecord{}, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return referrerMigrationRecord{}, errors.New("referrer migration state exceeds size limit")
	}
	var record referrerMigrationRecord
	if json.Unmarshal(data, &record) != nil || record.SchemaVersion != 1 || record.Authority != authority || record.Repository != repository || record.Subject != subject || record.Generation == 0 || !route.ValidDigest(record.Subject) || record.FallbackIndexDigest != "" && !route.ValidDigest(record.FallbackIndexDigest) {
		return referrerMigrationRecord{}, errors.New("referrer migration state is invalid")
	}
	if record.State == "" {
		record.State = referrerMigrationComplete
	}
	if record.State != referrerMigrationMigrating && record.State != referrerMigrationComplete || record.State == referrerMigrationComplete && !route.ValidDigest(record.NativeIndexDigest) || record.State == referrerMigrationMigrating && record.NativeIndexDigest != "" {
		return referrerMigrationRecord{}, errors.New("referrer migration state is invalid")
	}
	for _, digest := range record.Referrers {
		if !route.ValidDigest(digest) {
			return referrerMigrationRecord{}, errors.New("referrer migration state contains an invalid digest")
		}
	}
	record.version = object.Version
	return record, nil
}
