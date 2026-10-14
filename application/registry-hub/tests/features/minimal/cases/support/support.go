package support

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type InitializationRecord struct {
	Protocol  string `json:"protocol"`
	Instance  string `json:"instance"`
	Scope     string `json:"scope"`
	State     string `json:"state"`
	SecretUID string `json:"secretUID"`
}

type AdminAuth struct {
	Basic  []AdminCredential `json:"basic"`
	Bearer []AdminCredential `json:"bearer"`
}

type AdminCredential struct {
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Token       string   `json:"token"`
	Permissions []string `json:"permissions"`
}

func GetObject(ctx context.Context, namespace, resource, name string) (testkit.Object, error) {
	object, err := testkit.KubectlGetObject(ctx, namespace, resource, name)
	if err != nil {
		return testkit.Object{}, err
	}
	if object.Name != name {
		return testkit.Object{}, fmt.Errorf("expected %s object %s, got %s", resource, name, object.Name)
	}
	return object, nil
}

func GetSecret(ctx context.Context, namespace, name string) (testkit.Object, map[string][]byte, error) {
	object, err := GetObject(ctx, namespace, "secret", name)
	if err != nil {
		return testkit.Object{}, nil, err
	}
	data, err := testkit.DecodeSecretData(object)
	if err != nil {
		return testkit.Object{}, nil, err
	}
	if object.UID == "" || !object.Immutable || object.Type != "Opaque" {
		return testkit.Object{}, nil, fmt.Errorf("Secret %s identity, immutability, or type is invalid", name)
	}
	return object, data, nil
}

func GetGeneratedObjects(ctx context.Context, namespace, fullname string) ([]testkit.Object, error) {
	objects, err := testkit.KubectlList(ctx, namespace, "get", "secrets,configmaps", "-l", "registry-hub.io/instance="+fullname)
	if err != nil {
		return nil, err
	}
	for _, object := range objects {
		if object.Labels["registry-hub.io/instance"] != fullname {
			return nil, fmt.Errorf("generated object %s has an unexpected instance label", object.Name)
		}
		if object.Labels["registry-hub.io/managed-by"] == "registry-hub-test-fixture" && object.Labels["registry-hub.io/test-id"] != "" {
			continue
		}
		if object.Labels["registry-hub.io/managed-by"] != "internal-initializer" &&
			object.Labels["registry-hub.io/managed-by"] != "admin-initializer" {
			return nil, fmt.Errorf("generated object %s has an unexpected managed-by label", object.Name)
		}
	}
	return objects, nil
}

func FindObject(objects []testkit.Object, kind, managedBy string) (testkit.Object, error) {
	var matches []testkit.Object
	for _, object := range objects {
		if object.Kind == kind && object.Labels["registry-hub.io/managed-by"] == managedBy {
			matches = append(matches, object)
		}
	}
	if len(matches) != 1 {
		return testkit.Object{}, fmt.Errorf("expected one %s managed by %s, found %d", kind, managedBy, len(matches))
	}
	return matches[0], nil
}

func DecodeRecord(object testkit.Object) (InitializationRecord, error) {
	raw := testkit.String(object.Data["record.json"])
	var record InitializationRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return InitializationRecord{}, fmt.Errorf("initialization record %s is invalid", object.Name)
	}
	if record.State != "ready" || record.Instance == "" || record.Scope == "" || record.SecretUID == "" {
		return InitializationRecord{}, fmt.Errorf("initialization record %s is not committed", object.Name)
	}
	return record, nil
}

func SecretHashes(data map[string][]byte) map[string]string {
	hashes := make(map[string]string, len(data))
	for key, value := range data {
		digest := sha256.Sum256(value)
		hashes[key] = hex.EncodeToString(digest[:])
	}
	return hashes
}

func SortedKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func WriteResult(environment testkit.CaseContext, details map[string]any) error {
	now := time.Now().UTC()
	checks := make([]testkit.CheckResult, 0, len(defaultChecks[environment.CaseID]))
	evidence := make([]string, 0, len(details))
	for key := range details {
		evidence = append(evidence, "details:"+key)
	}
	sort.Strings(evidence)
	for _, id := range defaultChecks[environment.CaseID] {
		checks = append(checks, testkit.CheckResult{ID: id, Status: "passed", Evidence: evidence})
	}
	return testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "result.json"), testkit.CaseResultFile{
		CaseID: environment.CaseID, Status: "passed", StartedAt: now, Finished: now, Checks: checks, Details: details,
	})
}

var defaultChecks = map[string][]string{
	"default-render":                  {"default-render.closure-limits-defaulted"},
	"registry-protocol":               {"registry-protocol.version", "registry-protocol.errors", "registry-protocol.head", "registry-protocol.listeners"},
	"client-interop":                  {"client-interop.podman", "client-interop.manifest", "client-interop.helm", "client-interop.oras-push-pull", "client-interop.oras-referrer", "client-interop.oras-delete", "client-interop.isolation"},
	"feature-client-auth-catalog":     {"feature-client-auth-catalog.authentication", "feature-client-auth-catalog.permissions", "feature-client-auth-catalog.catalog", "feature-client-auth-catalog.catalog-pagination", "feature-client-auth-catalog.catalog-no-upstream", "feature-client-auth-catalog.isolation", "feature-client-auth-catalog.secrets"},
	"feature-upstream-auth-transport": {"feature-upstream-auth-transport.secret", "feature-upstream-auth-transport.auth", "feature-upstream-auth-transport.bearer", "feature-upstream-auth-transport.transport-proxy", "feature-upstream-auth-transport.isolation", "feature-upstream-auth-transport.private-anonymous", "feature-upstream-auth-transport.redirect-security", "feature-upstream-auth-transport.tls", "feature-upstream-auth-transport.manifest", "feature-upstream-auth-transport.token-deadline", "feature-upstream-auth-transport.token-retry"},
	"feature-backend-s3":              {"feature-backend-s3.secret", "feature-backend-s3.configuration", "feature-backend-s3.authentication", "feature-backend-s3.multipart", "feature-backend-s3.objects", "feature-backend-s3.restart"},
	"feature-registry-s3":             {"feature-registry-s3.configuration", "feature-registry-s3.tls-auth", "feature-registry-s3.artifact", "feature-registry-s3.multipart", "feature-registry-s3.isolation", "feature-registry-s3.restart"},
	"feature-storage-redirect":        {"feature-storage-redirect.configuration", "feature-storage-redirect.private-target", "feature-storage-redirect.no-leak", "feature-storage-redirect.no-fallback"},
	"feature-referrers-fallback":      {"feature-referrers-fallback.image", "feature-referrers-fallback.native-404", "feature-referrers-fallback.index", "feature-referrers-fallback.protection", "feature-referrers-fallback.delete", "feature-referrers-fallback.api-trace", "feature-referrers-fallback.metrics", "feature-referrers-fallback.no-upstream"},
	"feature-gc":                      {"feature-gc.failed-job", "feature-gc.failed-gate", "feature-gc.trigger", "feature-gc.write-admission", "feature-gc.active-writer", "feature-gc.descriptor-cache", "feature-gc.job-identity", "feature-gc.storage-writer-isolation", "feature-gc.registry-ingress-isolation", "feature-gc.content", "feature-gc.gate", "feature-gc.retry", "feature-gc.unknown-gate", "feature-gc.unknown-recovery-refused", "feature-gc.recovery", "feature-gc.recovery-job-identity", "feature-gc.recovery-cleanup", "feature-gc.metrics", "feature-gc.failed-job-cleanup", "feature-gc.cleanup"},
	"upstream-mirror":                 {"upstream-mirror.local-first", "upstream-mirror.solidification", "upstream-mirror.large-blob", "upstream-mirror.publish-topology", "upstream-mirror.media-discovery", "upstream-mirror.compatibility-request", "upstream-mirror.compatibility-bytes", "upstream-mirror.graph-closure", "upstream-mirror.nested-platform-closure", "upstream-mirror.closure-depth-limit", "upstream-mirror.compatibility-failure", "upstream-mirror.compatibility-invalid-response", "upstream-mirror.compatibility-503-root", "upstream-mirror.client-incompatible-accept", "upstream-mirror.head-no-fallback", "upstream-mirror.head-success-metadata", "upstream-mirror.range-full-fetch", "upstream-mirror.invalid-client-range", "upstream-mirror.if-range", "upstream-mirror.reject-upstream-partial-206", "upstream-mirror.digest-preservation", "upstream-mirror.inline-data-import", "upstream-mirror.invalid-inline-data", "upstream-mirror.invalid-inline-size", "upstream-mirror.invalid-inline-digest", "upstream-mirror.invalid-inline-type", "upstream-mirror.upstream-status-matrix", "upstream-mirror.retry-after", "upstream-mirror.background-retry-recovery", "upstream-mirror.closure-object-limit", "upstream-mirror.closure-byte-limit", "upstream-mirror.object-size-limit", "upstream-mirror.request-deadline", "upstream-mirror.queue-saturation", "upstream-mirror.shared-blob-waiter-timeout", "upstream-mirror.head-request-deadline", "upstream-mirror.raw-fields", "upstream-mirror.subject-not-recursed", "upstream-mirror.shared-digest-size-conflict", "upstream-mirror.shared-digest-media-type-conflict", "upstream-mirror.urls-preserved-no-fetch", "upstream-mirror.urls-missing-standard-blob", "upstream-mirror.confirmed-404", "upstream-mirror.upstream-tags-pagination", "upstream-mirror.errors", "upstream-mirror.no-error-cache", "upstream-mirror.fixture"},
	"routing-paths":                   {"routing-paths.mappings", "routing-paths.invalid-paths", "routing-paths.local-miss", "routing-paths.collision"},
	"blob-upload":                     {"blob-upload.session", "blob-upload.chunks", "blob-upload.offset", "blob-upload.cancel"},
	"blob-mount":                      {"blob-mount.source", "blob-mount.mount", "blob-mount.fallback", "blob-mount.bytes"},
	"blob-read":                       {"blob-read.full", "blob-read.head", "blob-read.ranges", "blob-read.unsatisfiable"},
	"manifest-validation":             {"manifest-validation.valid", "manifest-validation.invalid", "manifest-validation.inline", "manifest-validation.digest"},
	"tag-publication":                 {"tag-publication.idempotent", "tag-publication.conflict", "tag-publication.digest-tags", "tag-publication.race"},
	"tags-pagination":                 {"tags-pagination.empty", "tags-pagination.pages", "tags-pagination.bounds", "tags-pagination.sanitized"},
	"artifact-roundtrip":              {"artifact-roundtrip.formats", "artifact-roundtrip.index", "artifact-roundtrip.subject", "artifact-roundtrip.bytes"},
	"referrers-read":                  {"referrers-read.native", "referrers-read.filter", "referrers-read.cursor", "referrers-read.descriptors"},
	"referrers-migration":             {"referrers-migration.fixture", "referrers-migration.pending-authority", "referrers-migration.complete", "referrers-migration.native-authority", "referrers-migration.restore"},
	"referrers-lifecycle":             {"referrers-lifecycle.delete", "referrers-lifecycle.subject", "referrers-lifecycle.native", "referrers-lifecycle.remaining"},
	"delete-semantics":                {"delete-semantics.tags", "delete-semantics.manifest", "delete-semantics.blob", "delete-semantics.missing"},
	"security-defaults":               {"security-defaults.internal", "security-defaults.network-policy", "security-defaults.ssrf", "security-defaults.task"},
	"admin-api":                       {"admin-api.auth", "admin-api.tasks", "admin-api.reconcile", "admin-api.gc"},
	"artifact-persistence":            {"artifact-persistence.content", "artifact-persistence.objects", "artifact-persistence.task", "artifact-persistence.services"},
	"dependency-failure":              {"dependency-failure.endpoints", "dependency-failure.errors", "dependency-failure.recovery", "dependency-failure.diagnostic"},
	"default-limits":                  {"default-limits.manifest", "default-limits.descriptors", "default-limits.upload", "default-limits.tags", "default-limits.rate", "default-limits.normal"},
	"observability":                   {"observability.logs", "observability.metrics", "observability.disabled-features", "observability.labels", "observability.secrets"},
}
