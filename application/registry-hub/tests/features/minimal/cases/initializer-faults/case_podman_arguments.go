package initializerfaults

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/apimock"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (h *harness) podmanArguments() []string {
	identity := h.identity
	return []string{
		"run", "--rm", "--pull=never", "--network=host", "--user=0:0",
		"--volume", h.api.ServiceDir + ":/registry-hub-test-service-account:ro",
		"--env", "KUBERNETES_SERVICE_HOST=127.0.0.1",
		"--env", fmt.Sprintf("KUBERNETES_SERVICE_PORT_HTTPS=%d", h.api.Port),
		"--env", "KUBERNETES_SERVICE_ACCOUNT_DIR=/registry-hub-test-service-account",
		"--env", "POD_NAMESPACE=develop",
		"--env", "REGISTRY_HUB_INIT_INSTANCE=" + identity.Instance,
		"--env", "REGISTRY_HUB_INIT_SECRET_NAME=" + identity.InternalSecret,
		"--env", "REGISTRY_HUB_INIT_RECORD_NAME=" + identity.InternalRecord,
		"--env", "REGISTRY_HUB_INIT_SCOPE=" + scopeHash("internal/"+identity.Instance),
		"--env", "REGISTRY_HUB_INIT_SERVICE_NAME=" + identity.Instance + "-registry",
		"--env", "REGISTRY_HUB_INIT_AUTH=true",
		"--env", "REGISTRY_HUB_INIT_TLS=true",
		"--env", "REGISTRY_HUB_INIT_HTTP_SECRET=true",
		"--env", "REGISTRY_HUB_INIT_AUTH_KEY=config.json",
		"--env", "REGISTRY_HUB_INIT_HTPASSWD_KEY=htpasswd",
		"--env", "REGISTRY_HUB_INIT_CA_KEY=ca.crt",
		"--env", "REGISTRY_HUB_INIT_CERTIFICATE_KEY=tls.crt",
		"--env", "REGISTRY_HUB_INIT_PRIVATE_KEY_KEY=tls.key",
		"--env", "REGISTRY_HUB_INIT_HTTP_SECRET_KEY=http-secret",
		"--env", "REGISTRY_HUB_INIT_ADMIN_AUTH=true",
		"--env", "REGISTRY_HUB_INIT_ADMIN_SECRET_NAME=" + identity.AdminSecret,
		"--env", "REGISTRY_HUB_INIT_ADMIN_RECORD_NAME=" + identity.AdminRecord,
		"--env", "REGISTRY_HUB_INIT_ADMIN_SCOPE=" + scopeHash("admin/"+identity.Instance),
		"--env", "REGISTRY_HUB_INIT_ADMIN_KEY=config.json",
		h.environment.ImageReference, "initialize",
	}
}

func verifyPersistedStateScenario(ctx context.Context, h *harness, scenario string) (map[string]any, error) {
	if err := h.runOne(ctx); err != nil {
		return nil, err
	}
	before, exists := h.api.Object("secrets", h.identity.InternalSecret)
	if !exists {
		return nil, fmt.Errorf("initialization did not persist the internal Secret")
	}
	secretCreates := h.api.Stats().CreateSecrets
	switch scenario {
	case "missing-ready-secret":
		h.api.DeleteObject("secrets", h.identity.InternalSecret)
		if err := h.expectFailure(ctx, "refusing to regenerate credentials"); err != nil {
			return nil, err
		}
	case "corrupt-ready-secret":
		h.api.SetSecretData(h.identity.InternalSecret, "config.json", base64.StdEncoding.EncodeToString([]byte("{}")))
		if err := h.expectFailure(ctx, "Secret is invalid"); err != nil {
			return nil, err
		}
	case "invalid-tls-certificate":
		h.api.SetSecretData(h.identity.InternalSecret, "tls.crt", base64.StdEncoding.EncodeToString([]byte("invalid")))
		if err := h.expectFailure(ctx, "invalid PEM"); err != nil {
			return nil, err
		}
	case "wrong-tls-san", "expired-tls-certificate":
		caPEM, certificatePEM, privateKeyPEM, err := invalidTLSChain(
			h.identity.Instance+"-registry", scenario == "expired-tls-certificate", scenario == "wrong-tls-san",
		)
		if err != nil {
			return nil, err
		}
		h.api.SetSecretData(h.identity.InternalSecret, "ca.crt", base64.StdEncoding.EncodeToString(caPEM))
		h.api.SetSecretData(h.identity.InternalSecret, "tls.crt", base64.StdEncoding.EncodeToString(certificatePEM))
		h.api.SetSecretData(h.identity.InternalSecret, "tls.key", base64.StdEncoding.EncodeToString(privateKeyPEM))
		if err := h.expectFailure(ctx, "verify internal TLS certificate"); err != nil {
			return nil, err
		}
	case "changed-secret-uid":
		h.api.SetSecretUID(h.identity.InternalSecret, "replacement-secret-uid")
		if err := h.expectFailure(ctx, "identity changed"); err != nil {
			return nil, err
		}
	case "missing-record-recovery":
		h.api.DeleteObject("configmaps", h.identity.InternalRecord)
		if err := h.runOne(ctx); err != nil {
			return nil, err
		}
		after, exists := h.api.Object("secrets", h.identity.InternalSecret)
		if !exists || !sameSecret(before, after) || recordStatus(h.api, h.identity.InternalRecord) != "ready" {
			return nil, fmt.Errorf("record recovery did not preserve the internal Secret")
		}
	case "incompatible-record-scope":
		h.api.SetRecordScope(h.identity.InternalRecord, scopeHash("wrong-scope"))
		if err := h.expectFailure(ctx, "different instance or scope"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown persisted state scenario %q", scenario)
	}
	if scenario != "missing-ready-secret" && h.api.Stats().CreateSecrets != secretCreates {
		return nil, fmt.Errorf("scenario %s attempted to regenerate a committed Secret", scenario)
	}
	if scenario == "missing-ready-secret" && h.api.Stats().CreateSecrets != secretCreates {
		return nil, fmt.Errorf("missing ready Secret was silently recreated")
	}
	return resultSummary(h, h.api.Stats()), nil
}

func verifyReadyState(h *harness) error {
	for _, pair := range [][2]string{{"secrets", h.identity.InternalSecret}, {"secrets", h.identity.AdminSecret}, {"configmaps", h.identity.InternalRecord}, {"configmaps", h.identity.AdminRecord}} {
		if _, exists := h.api.Object(pair[0], pair[1]); !exists {
			return fmt.Errorf("initializer did not persist %s %s", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{h.identity.InternalRecord, h.identity.InternalSecret}, {h.identity.AdminRecord, h.identity.AdminSecret}} {
		object, _ := h.api.Object("configmaps", pair[0])
		data := testkit.Map(object["data"])
		var record map[string]any
		if err := json.Unmarshal([]byte(testkit.String(data["record.json"])), &record); err != nil {
			return fmt.Errorf("initialization record %s is invalid", pair[0])
		}
		secret, _ := h.api.Object("secrets", pair[1])
		metadata := testkit.Map(secret["metadata"])
		if testkit.String(record["state"]) != "ready" || testkit.String(record["secretUID"]) != testkit.String(metadata["uid"]) {
			return fmt.Errorf("initialization record %s does not bind the persisted Secret UID", pair[0])
		}
	}
	return nil
}

func recordStatus(api *apimock.Server, name string) string {
	object, exists := api.Object("configmaps", name)
	if !exists {
		return ""
	}
	data := testkit.Map(object["data"])
	var record map[string]any
	if json.Unmarshal([]byte(testkit.String(data["record.json"])), &record) != nil {
		return "invalid"
	}
	return testkit.String(record["state"])
}

func resultSummary(h *harness, stats apimock.Stats) map[string]any {
	secrets := make([]map[string]any, 0, 2)
	for _, name := range []string{h.identity.InternalSecret, h.identity.AdminSecret} {
		if object, exists := h.api.Object("secrets", name); exists {
			secrets = append(secrets, secretSummary(object))
		}
	}
	return map[string]any{"api": stats, "secrets": secrets}
}

func secretSummary(object map[string]any) map[string]any {
	metadata := testkit.Map(object["metadata"])
	encoded := testkit.Map(object["data"])
	hashes := make(map[string]string, len(encoded))
	for key, raw := range encoded {
		value, err := base64.StdEncoding.DecodeString(testkit.String(raw))
		if err != nil {
			hashes[key] = "invalid-base64"
			continue
		}
		digest := sha256.Sum256(value)
		hashes[key] = hex.EncodeToString(digest[:])
	}
	return map[string]any{"name": metadata["name"], "uid": metadata["uid"], "keySHA256": hashes}
}

func sameSecret(left, right map[string]any) bool {
	if left == nil || right == nil {
		return false
	}
	return reflectJSON(secretSummary(left)) == reflectJSON(secretSummary(right))
}

func reflectJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func testIdentity(environment testkit.CaseContext) identity {
	instance := "rh-fault-" + environment.RunID
	return identity{
		Instance: instance, InternalSecret: instance + "-registry-internal",
		InternalRecord: instance + "-registry-initialization",
		AdminSecret:    instance + "-admin-auth", AdminRecord: instance + "-admin-auth-initialization",
	}
}

func scopeHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
