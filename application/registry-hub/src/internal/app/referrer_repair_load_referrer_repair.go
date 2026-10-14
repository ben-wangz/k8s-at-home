package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

func (s *Service) loadReferrerRepair(ctx context.Context, key string) (*referrerRepairIntent, error) {
	for attempt := 0; attempt < 16; attempt++ {
		object, err := s.store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil || len(data) > 1<<20 {
			return nil, errors.New("referrer repair state exceeds size limit")
		}
		var intent referrerRepairIntent
		if json.Unmarshal(data, &intent) != nil || !validReferrerRepair(intent) || key != referrerRepairKey(intent.Authority, intent.Repository, intent.Subject, intent.Referrer) {
			return nil, errors.New("referrer repair state has invalid identity or schema")
		}
		switch intent.SchemaVersion {
		case referrerRepairSchemaCurrent:
			intent.version = object.Version
			return &intent, nil
		case referrerRepairSchemaPrevious:
			intent.SchemaVersion = referrerRepairSchemaCurrent
			updated, marshalErr := json.Marshal(intent)
			if marshalErr != nil {
				return nil, marshalErr
			}
			entry, putErr := s.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return nil, putErr
			}
			intent.version = entry.Version
			return &intent, nil
		default:
			return nil, errors.New("referrer repair state has invalid identity or schema")
		}
	}
	return nil, storage.ErrUnavailable
}

func validReferrerRepair(intent referrerRepairIntent) bool {
	validIdentity := route.ValidateRepository(intent.Repository) == nil
	if intent.Authority != "" {
		validIdentity = validIdentity && route.ValidateAuthority(intent.Authority) == nil
	}
	if intent.SchemaVersion != referrerRepairSchemaPrevious && intent.SchemaVersion != referrerRepairSchemaCurrent || !validIdentity || !route.ValidDigest(intent.Subject) || !route.ValidDigest(intent.Referrer) || intent.Operation != "add" && intent.Operation != "remove" || intent.Generation == 0 {
		return false
	}
	for _, taskID := range intent.TaskIDs {
		if len(taskID) != 32 {
			return false
		}
		if _, err := hex.DecodeString(taskID); err != nil {
			return false
		}
	}
	return true
}

func referrerRepairKey(authority, repository, subject, referrer string) string {
	return referrerRepairSubjectPrefix(authority, repository, subject) + objectLeaseKey(authority, repository, referrer) + ".json"
}

func referrerRepairSubjectPrefix(authority, repository, subject string) string {
	return referrerRepairPrefix + objectLeaseKey(authority, repository, subject) + "/"
}

func referrerGenerationReference(subject, referrer string) string {
	return "referrers\x00" + subject + "\x00" + referrer
}

func referrerRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 11 {
		attempt = 11
	}
	delay := time.Second << (attempt - 1)
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func replaceReferrerDescriptors(current, desired []json.RawMessage) ([]json.RawMessage, bool, error) {
	currentMap, err := referrerDescriptorMap(current)
	if err != nil {
		return nil, false, err
	}
	desiredMap, err := referrerDescriptorMap(desired)
	if err != nil {
		return nil, false, err
	}
	if len(currentMap) == len(desiredMap) {
		equal := true
		for digest, descriptor := range currentMap {
			if !bytes.Equal(descriptor, desiredMap[digest]) {
				equal = false
				break
			}
		}
		if equal {
			return sortedReferrerDescriptors(desiredMap), false, nil
		}
	}
	for digest, descriptor := range currentMap {
		if wanted, found := desiredMap[digest]; found && !bytes.Equal(descriptor, wanted) {
			return nil, false, invalidReferrer("referrer digest has conflicting descriptor metadata")
		}
	}
	return sortedReferrerDescriptors(desiredMap), true, nil
}

func referrerDescriptorMap(items []json.RawMessage) (map[string]json.RawMessage, error) {
	values := make(map[string]json.RawMessage, len(items))
	for _, item := range items {
		var descriptor referrerDescriptor
		if json.Unmarshal(item, &descriptor) != nil || !route.ValidDigest(descriptor.Digest) {
			return nil, invalidReferrer("referrer descriptor is invalid")
		}
		canonical, err := canonicalDescriptor(item)
		if err != nil {
			return nil, invalidReferrer("referrer descriptor is invalid")
		}
		if previous, found := values[descriptor.Digest]; found {
			if !bytes.Equal(previous, canonical) {
				return nil, invalidReferrer("referrer list contains conflicting duplicate descriptors")
			}
			continue
		}
		values[descriptor.Digest] = canonical
	}
	return values, nil
}

func (s *Service) replaceFallbackReferrers(ctx context.Context, routed route.Route, subject string, desired []json.RawMessage) error {
	desiredMap, err := referrerDescriptorMap(desired)
	if err != nil {
		return err
	}
	releaseGate, err := s.gate.Enter(ctx)
	if err != nil {
		return err
	}
	defer releaseGate()
	leaseCtx, subjectLease, releaseSubject, err := s.acquireReferrerLease(ctx, routed, subject)
	if err != nil {
		return err
	}
	defer releaseSubject()
	authority, repository := routeStorageIdentity(routed)
	tag := referrerFallbackTag(subject)
	tagCtx, tagLease, releaseTag, err := acquireRenewedLease(leaseCtx, s.tasks.Leases(), tagLeaseKey(authority, repository, tag), requestID(), 60*time.Second)
	if err != nil {
		return err
	}
	defer releaseTag()
	current, err := s.readFallbackIndex(tagCtx, routed, subject, tag)
	corrupt := false
	if err != nil {
		var permanent *referrerPermanentError
		if !errors.As(err, &permanent) {
			return err
		}
		current.manifests = []json.RawMessage{}
		corrupt = true
	}
	if _, _, err := replaceReferrerDescriptors(current.manifests, sortedReferrerDescriptors(desiredMap)); err != nil {
		return err
	}
	var repairDigests []string
	if !corrupt {
		currentMap, _ := referrerDescriptorMap(current.manifests)
		changes := make(map[string]string)
		for digest := range currentMap {
			if _, found := desiredMap[digest]; !found {
				changes[digest] = "remove"
			}
		}
		for digest := range desiredMap {
			if _, found := currentMap[digest]; !found {
				changes[digest] = "add"
			}
		}
		digests := make([]string, 0, len(changes))
		for digest := range changes {
			digests = append(digests, digest)
		}
		sort.Strings(digests)
		repairDigests = digests
		for _, digest := range digests {
			operation := changes[digest]
			if _, err := s.beginReferrerRepair(leaseCtx, routed, subject, digest, operation, false, ""); err != nil {
				return err
			}
		}
	}
	writeErr := s.writeFallbackReferrers(leaseCtx, subjectLease, tagCtx, tagLease, routed, subject, tag, func(current []json.RawMessage) ([]json.RawMessage, bool, error) {
		return replaceReferrerDescriptors(current, sortedReferrerDescriptors(desiredMap))
	}, corrupt)
	if writeErr != nil {
		if !corrupt {
			for _, digest := range repairDigests {
				if intent, loadErr := s.loadReferrerRepair(leaseCtx, referrerRepairKey(authority, repository, subject, digest)); loadErr == nil {
					_ = s.recordReferrerRepairFailure(leaseCtx, intent, writeErr)
				}
			}
		}
		return writeErr
	}
	if err := s.clearAllReferrerRepairs(leaseCtx, routed, subject, desiredMap); err != nil {
		return err
	}
	return nil
}
