package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

func (s *Service) referrerMigrationPending(ctx context.Context, routed route.Route, subject string, nativeAvailable bool) (bool, error) {
	authority, repository := routeStorageIdentity(routed)
	record, err := s.loadReferrerMigration(ctx, authority, repository, subject)
	if errors.Is(err, storage.ErrNotFound) {
		if !nativeAvailable {
			return false, nil
		}
		fallback, fallbackErr := s.readFallbackIndex(ctx, routed, subject, referrerFallbackTag(subject))
		if fallbackErr != nil {
			return false, fallbackErr
		}
		return fallback.digest != "", nil
	}
	if err != nil {
		return false, err
	}
	return record.State == referrerMigrationMigrating, nil
}

func referrerMigrationKey(authority, repository, subject string) string {
	digest := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + subject))
	return "referrers/migrations/" + hex.EncodeToString(digest[:]) + ".json"
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if other, found := right[key]; !found || other != value {
			return false
		}
	}
	return true
}
