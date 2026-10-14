package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"strconv"
)

func (s *Service) getReferrerReconcile(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !onlyQueryKeys(query, "repository", "subject", "limit", "lastReferrer", "lastIntent") {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile query is invalid")
		return
	}
	routed, err := reconcileRoute(query.Get("repository"), query.Get("subject"))
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "repository or subject is invalid")
		return
	}
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile page size is invalid")
			return
		}
	}
	lastReferrer, lastIntent := query.Get("lastReferrer"), query.Get("lastIntent")
	if lastReferrer != "" && !route.ValidDigest(lastReferrer) || lastIntent != "" && !route.ValidDigest(lastIntent) {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile cursor is invalid")
		return
	}
	native, err := s.nativeReferrersAvailable(ctx, routed, query.Get("subject"))
	if err != nil {
		writeAdminError(writer, http.StatusBadGateway, "UNKNOWN", "referrers capability could not be determined")
		return
	}
	migrationPending, pendingErr := s.referrerMigrationPending(ctx, routed, query.Get("subject"), native)
	if pendingErr != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer migration state could not be read")
		return
	}
	authority, repository := routeStorageIdentity(routed)
	migration, migrationErr := s.loadReferrerMigration(ctx, authority, repository, query.Get("subject"))
	if migrationErr != nil && !errors.Is(migrationErr, storage.ErrNotFound) {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer migration state could not be read")
		return
	}
	var index referrerIndex
	if native && !migrationPending {
		index, err = s.fetchCompleteLocalReferrers(ctx, routed, query.Get("subject"))
	} else {
		tag := referrerFallbackTag(query.Get("subject"))
		index, err = s.readFallbackIndex(ctx, routed, query.Get("subject"), tag)
	}
	if err != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer index could not be read")
		return
	}
	descriptors, nextReferrer := pageAdminDescriptors(index.manifests, lastReferrer, limit)
	intents, nextIntent, err := referrerIntentPage(ctx, s.store, routed, query.Get("subject"), lastIntent, limit)
	if err != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer repair state could not be read")
		return
	}
	var migrationView adminReferrerMigrationView
	if migrationErr == nil {
		migrationView = adminReferrerMigrationView{
			"state": migration.State, "generation": migration.Generation, "referrers": len(migration.Referrers),
			"fallbackIndexDigest": migration.FallbackIndexDigest,
			"nativeIndexDigest":   migration.NativeIndexDigest, "updatedAt": migration.UpdatedAt,
		}
	} else if migrationPending {
		migrationView = adminReferrerMigrationView{"state": "migration_required"}
	}
	writeJSON(writer, http.StatusOK, adminReferrerReconcileResponse{
		Repository: query.Get("repository"), Subject: query.Get("subject"),
		Source: reconcileSource(native && !migrationPending), IndexDigest: index.digest,
		Referrers: adminReferrerDescriptors(descriptors), NextReferrer: nextReferrer,
		RepairIntents: publicReferrerIntents(intents), NextIntent: nextIntent,
		LastMigration: migrationView,
	})
}

func (s *Service) retrySubjectReferrers(writer http.ResponseWriter, ctx context.Context, routed route.Route) bool {
	items, _, err := referrerIntentPage(ctx, s.store, routed, routed.Reference, "", 4096)
	if err != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer repair state could not be read")
		return false
	}
	for index := range items {
		intent, err := s.loadReferrerRepair(ctx, referrerRepairKey(items[index].Authority, items[index].Repository, items[index].Subject, items[index].Referrer))
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer repair state changed during retry")
			return false
		}
		if err := s.retryReferrerIntent(ctx, intent); err != nil {
			writeAdminError(writer, http.StatusConflict, "CONFLICT", "referrer repair could not be scheduled")
			return false
		}
	}
	writeJSON(writer, http.StatusAccepted, adminReferrerRetryResponse{Action: "retry", Scheduled: len(items)})
	return true
}

func (s *Service) reconcileExplicit(writer http.ResponseWriter, ctx context.Context, routed route.Route, subject, action string, digests []string, native bool) bool {
	for _, digest := range digests {
		if !route.ValidDigest(digest) {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer digest is invalid")
			return true
		}
		if action == "add" {
			descriptor, err := s.localReferrerDescriptor(ctx, routed, digest, subject)
			if err != nil {
				writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer manifest failed validation")
				return true
			}
			if native {
				release, err := s.gate.Enter(ctx)
				if err != nil {
					writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "writes are temporarily unavailable")
					return true
				}
				intent := &referrerRepairIntent{Authority: routed.Upstream, Repository: routed.RemoteRepo, Subject: subject, Referrer: digest}
				publishErr := s.publishLocalReferrerToNative(ctx, routed, intent)
				release()
				if publishErr != nil {
					writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "native referrer publication failed")
					return true
				}
				continue
			}
			if err := s.updateFallbackReferrers(ctx, routed, subject, descriptor, false); err != nil {
				writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer add failed")
				return true
			}
			continue
		}
		descriptor, _ := json.Marshal(map[string]string{"digest": digest})
		if err := s.updateFallbackReferrers(ctx, routed, subject, descriptor, true); err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer remove failed")
			return true
		}
	}
	return false
}

func (s *Service) fetchCompleteLocalReferrers(ctx context.Context, routed route.Route, subject string) (referrerIndex, error) {
	routed.Endpoint = route.EndpointReferrers
	routed.Reference = subject
	path := registryPath(routed)
	first, err := s.registry.Do(ctx, http.MethodGet, path, nil, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		return referrerIndex{}, err
	}
	if first.StatusCode != http.StatusOK {
		status := first.StatusCode
		_ = first.Body.Close()
		return referrerIndex{}, registryStatusError("native Referrers GET", status)
	}
	return s.collectLocalReferrers(ctx, routed, first, "")
}

func (s *Service) validateReferrerCandidates(ctx context.Context, routed route.Route, subject string, digests []string) ([]json.RawMessage, error) {
	if len(digests) > s.cfg.Limits.MaxDescriptors {
		return nil, errors.New("referrer candidate count exceeds configured limit")
	}
	seen := make(map[string]struct{}, len(digests))
	result := make([]json.RawMessage, 0, len(digests))
	for _, digest := range digests {
		if !route.ValidDigest(digest) {
			return nil, errors.New("referrer candidate digest is invalid")
		}
		if _, found := seen[digest]; found {
			return nil, errors.New("referrer candidate list contains duplicates")
		}
		seen[digest] = struct{}{}
		descriptor, err := s.localReferrerDescriptor(ctx, routed, digest, subject)
		if err != nil {
			return nil, err
		}
		result = append(result, descriptor)
	}
	return result, nil
}

func pageAdminDescriptors(values []json.RawMessage, last string, limit int) ([]json.RawMessage, string) {
	descriptors := append([]json.RawMessage(nil), values...)
	sort.Slice(descriptors, func(i, j int) bool {
		var left, right referrerDescriptor
		_ = json.Unmarshal(descriptors[i], &left)
		_ = json.Unmarshal(descriptors[j], &right)
		return left.Digest < right.Digest
	})
	filtered := make([]json.RawMessage, 0, limit+1)
	for _, raw := range descriptors {
		var item referrerDescriptor
		if json.Unmarshal(raw, &item) == nil && item.Digest > last {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) <= limit {
		return filtered, ""
	}
	var next referrerDescriptor
	_ = json.Unmarshal(filtered[limit-1], &next)
	return filtered[:limit], next.Digest
}

func publicReferrerIntents(items []referrerRepairIntent) []adminReferrerIntentView {
	result := make([]adminReferrerIntentView, 0, len(items))
	for _, item := range items {
		result = append(result, adminReferrerIntentView{
			Referrer: item.Referrer, Operation: item.Operation,
			Retryable: item.Retryable, Attempts: item.Attempts,
			NextAttemptAt: item.NextAttemptAt, ErrorClass: item.ErrorClass,
			Generation: item.Generation,
		})
	}
	return result
}

func reconcileSource(native bool) string {
	if native {
		return "native"
	}
	return "fallback"
}
