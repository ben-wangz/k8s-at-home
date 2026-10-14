package app

import (
	"context"
	"encoding/json"
	"net/http"
	"registry-hub/internal/route"
)

type referrerReconcileRequest struct {
	Repository string   `json:"repository" doc:"Repository name, optionally prefixed by its registry authority."`
	Subject    string   `json:"subject" doc:"Subject manifest digest."`
	Action     string   `json:"action" doc:"Reconciliation action: add, remove, replace, migrate, or retry."`
	Referrers  []string `json:"referrers,omitempty" doc:"Candidate referrer manifest digests. Required for add and remove; bounded for replace."`
	Source     string   `json:"source,omitempty" doc:"Use native to collect candidates from the native Referrers API during replace."`
	Complete   bool     `json:"complete,omitempty" doc:"Confirm that the candidate set is complete for replace or migrate."`
}

func (s *Service) reconcileReferrers(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if request.Method == http.MethodGet {
		s.getReferrerReconcile(writer, request, ctx)
		return
	}
	result := "failure"
	defer func() {
		if s.metrics != nil {
			s.metrics.recordReferrerReconcile(result)
		}
	}()
	var input referrerReconcileRequest
	if err := decodeAdminBody(writer, request, &input); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile request is invalid")
		return
	}
	routed, err := reconcileRoute(input.Repository, input.Subject)
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "repository or subject is invalid")
		return
	}
	if input.Action == "retry" {
		if input.Complete || input.Source != "" || len(input.Referrers) != 0 {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "retry does not accept a candidate list")
			return
		}
	}
	if input.Source != "" && input.Source != "native" {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile source is invalid")
		return
	}
	native, err := s.nativeReferrersAvailable(ctx, routed, input.Subject)
	if err != nil {
		writeAdminError(writer, http.StatusBadGateway, "UNKNOWN", "referrers capability could not be determined")
		return
	}
	migrationPending, err := s.referrerMigrationPending(ctx, routed, input.Subject, native)
	if err != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer migration state could not be read")
		return
	}
	writeNative := native && !migrationPending
	switch input.Action {
	case "add", "remove":
		if input.Complete || input.Source != "" || len(input.Referrers) == 0 || len(input.Referrers) > 128 {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer list or completeness flag is invalid")
			return
		}
		if native && input.Action == "remove" {
			writeAdminError(writer, http.StatusConflict, "CONFLICT", "native Referrers API cannot unlink a manifest without deleting it")
			return
		}
		if s.reconcileExplicit(writer, ctx, routed, input.Subject, input.Action, input.Referrers, writeNative) {
			return
		}
		writeJSON(writer, http.StatusAccepted, adminReferrerMutationResponse{
			Action: input.Action, Processed: len(input.Referrers), Source: reconcileSource(writeNative),
		})
		result = "success"
	case "replace":
		if migrationPending {
			writeAdminError(writer, http.StatusConflict, "CONFLICT", "fallback referrer migration must finish before replacing its authoritative index")
			return
		}
		if !input.Complete || len(input.Referrers) > 512 || input.Source == "native" && len(input.Referrers) != 0 || input.Source == "" && len(input.Referrers) == 0 && input.Complete == false {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "replace requires a complete, bounded candidate set")
			return
		}
		var digests []string
		if input.Source == "native" {
			if !native {
				writeAdminError(writer, http.StatusConflict, "CONFLICT", "native Referrers API is not available for this repository")
				return
			}
			index, collectErr := s.fetchCompleteLocalReferrers(ctx, routed, input.Subject)
			if collectErr != nil {
				writeAdminError(writer, http.StatusBadGateway, "UNKNOWN", "complete native referrer set could not be collected")
				return
			}
			for _, raw := range index.manifests {
				var descriptor referrerDescriptor
				if json.Unmarshal(raw, &descriptor) != nil || !route.ValidDigest(descriptor.Digest) {
					writeAdminError(writer, http.StatusBadGateway, "UNKNOWN", "native referrer descriptor is invalid")
					return
				}
				digests = append(digests, descriptor.Digest)
			}
		} else {
			digests = input.Referrers
		}
		descriptors, validateErr := s.validateReferrerCandidates(ctx, routed, input.Subject, digests)
		if validateErr != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "candidate referrer manifest failed validation")
			return
		}
		if err := s.replaceFallbackReferrers(ctx, routed, input.Subject, descriptors); err != nil {
			if referrerRetryable(err) {
				writer.Header().Set("Retry-After", "5")
				writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrers index replacement failed")
			} else {
				writeAdminError(writer, http.StatusConflict, "CONFLICT", "fallback referrers index cannot be replaced")
			}
			return
		}
		writeJSON(writer, http.StatusOK, adminReferrerReplaceResponse{
			Action: "replace", Complete: true, Referrers: len(descriptors), Source: input.SourceOrExplicit(),
		})
		result = "success"
	case "migrate":
		if !input.Complete || input.Source != "" || len(input.Referrers) != 0 {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "migrate requires complete=true and does not accept a candidate list")
			return
		}
		if !native {
			writeAdminError(writer, http.StatusConflict, "CONFLICT", "native Referrers API is not available for this repository")
			return
		}
		record, migrateErr := s.migrateFallbackReferrers(ctx, routed, input.Subject)
		if migrateErr != nil {
			if referrerRetryable(migrateErr) {
				writer.Header().Set("Retry-After", "5")
				writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrers migration is incomplete and can be retried")
			} else {
				writeAdminError(writer, http.StatusConflict, "CONFLICT", "fallback referrers migration could not be verified")
			}
			return
		}
		writeJSON(writer, http.StatusOK, adminReferrerMigrationResponse{
			Action: "migrate", Complete: true, Generation: record.Generation,
			Referrers: len(record.Referrers), NativeIndexDigest: record.NativeIndexDigest,
			FallbackIndexDigest: record.FallbackIndexDigest,
		})
		result = "success"
	case "retry":
		if migrationPending {
			writeAdminError(writer, http.StatusConflict, "CONFLICT", "fallback referrer migration must finish before referrer repair retry")
			return
		}
		if s.retrySubjectReferrers(writer, ctx, routed) {
			result = "success"
		}
	default:
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "referrer reconcile action is unsupported")
	}
}

func (input referrerReconcileRequest) SourceOrExplicit() string {
	if input.Source != "" {
		return input.Source
	}
	return "explicit"
}
