package externalcombinations

import (
	"context"
	"embed"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
)

//go:embed case.json
var metadata embed.FS

type secretSnapshot struct {
	Name   string            `json:"name"`
	UID    string            `json:"uid"`
	Hashes map[string]string `json:"keySHA256"`
}

type recordSnapshot struct {
	Name   string `json:"name"`
	UID    string `json:"uid"`
	SHA256 string `json:"sha256"`
}

type generatedSnapshot struct {
	Internal       secretSnapshot `json:"internalSecret"`
	Admin          secretSnapshot `json:"adminSecret"`
	InternalRecord recordSnapshot `json:"internalRecord"`
	AdminRecord    recordSnapshot `json:"adminRecord"`
}

type fixture struct {
	Name   string
	Data   map[string][]byte
	Object secretSnapshot
}

type mode struct {
	Name         string
	Values       map[string]any
	InternalAuth bool
	InternalTLS  bool
	InternalHTTP bool
	AdminAuth    bool
	NoInitJob    bool
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "external-combinations")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	manifestDir := filepath.Join(environment.OutputDir, "secret-manifests")
	if err := os.RemoveAll(manifestDir); err != nil {
		return err
	}
	defer os.RemoveAll(manifestDir)
	if err := testkit.EnsurePrivateDir(manifestDir); err != nil {
		return err
	}

	baseline, internalData, adminData, err := captureGenerated(ctx, environment)
	if err != nil {
		return err
	}
	fixtures, err := createFixtures(ctx, environment, manifestDir, internalData, adminData)
	if err != nil {
		return err
	}
	fixtureBaseline := fixtureSnapshots(fixtures)
	adminName, authName, htpasswdName := fixtureName(environment, "admin"), fixtureName(environment, "auth"), fixtureName(environment, "htpasswd")
	tlsName, httpName := fixtureName(environment, "tls"), fixtureName(environment, "http")
	modes := []mode{
		{
			Name: "admin-external-internal-automatic", InternalAuth: true, InternalTLS: true, InternalHTTP: true,
			Values: map[string]any{"backend": map[string]any{"adminAuth": map[string]any{"existingSecret": adminName}}},
		},
		{
			Name: "internal-external-admin-automatic", AdminAuth: true,
			Values: internalExternalValues(authName, htpasswdName, tlsName, httpName),
		},
		{
			Name: "all-external", NoInitJob: true,
			Values: mergeValues(internalExternalValues(authName, htpasswdName, tlsName, httpName), map[string]any{
				"backend": map[string]any{"adminAuth": map[string]any{"existingSecret": adminName}},
			}),
		},
	}
	modeResults := make([]map[string]any, 0, len(modes)+1)
	for _, current := range modes {
		if err := upgrade(ctx, environment, current.Name, current.Values); err != nil {
			return err
		}
		if err := verifyInitMode(ctx, environment, current); err != nil {
			return fmt.Errorf("configuration mode %s: %w", current.Name, err)
		}
		if err := verifyGeneratedUnchanged(ctx, environment, baseline); err != nil {
			return fmt.Errorf("configuration mode %s changed generated state: %w", current.Name, err)
		}
		if err := verifyFixturesUnchanged(ctx, environment, fixtureBaseline); err != nil {
			return fmt.Errorf("configuration mode %s changed external Secret data: %w", current.Name, err)
		}
		modeResults = append(modeResults, map[string]any{"mode": current.Name, "initializationJob": !current.NoInitJob})
	}
	if err := upgradeWithReuseValues(ctx, environment); err != nil {
		return err
	}
	legacyMode := mode{Name: "legacy-stored-external-references", NoInitJob: true}
	if err := verifyInitMode(ctx, environment, legacyMode); err != nil {
		return fmt.Errorf("legacy external-reference upgrade: %w", err)
	}
	if err := verifyExternalReferences(ctx, environment, adminName, authName, htpasswdName, tlsName, httpName); err != nil {
		return fmt.Errorf("legacy external-reference upgrade: %w", err)
	}
	if err := verifyGeneratedUnchanged(ctx, environment, baseline); err != nil {
		return fmt.Errorf("legacy external-reference upgrade changed generated state: %w", err)
	}
	if err := verifyFixturesUnchanged(ctx, environment, fixtureBaseline); err != nil {
		return fmt.Errorf("legacy external-reference upgrade changed external Secrets: %w", err)
	}
	modeResults = append(modeResults, map[string]any{
		"mode": "legacy-stored-external-references", "initializationJob": false,
		"upgradeStrategy": "helm-reuse-values", "prunedInitializationResources": true,
	})

	if err := upgrade(ctx, environment, "restore-defaults", nil); err != nil {
		return err
	}
	defaultMode := mode{Name: "default-restored", InternalAuth: true, InternalTLS: true, InternalHTTP: true, AdminAuth: true}
	if err := verifyInitMode(ctx, environment, defaultMode); err != nil {
		return fmt.Errorf("default configuration was not restored: %w", err)
	}
	if err := verifyGeneratedUnchanged(ctx, environment, baseline); err != nil {
		return fmt.Errorf("restoring the default configuration changed generated state: %w", err)
	}
	if err := verifyFixturesUnchanged(ctx, environment, fixtureBaseline); err != nil {
		return fmt.Errorf("restoring defaults changed external Secret data: %w", err)
	}
	if err := verifyServiceAccess(ctx, environment, adminData); err != nil {
		return err
	}
	modeResults = append(modeResults, map[string]any{"mode": "default-restored", "initializationJob": true})
	return support.WriteResult(environment, map[string]any{
		"modes":               modeResults,
		"generatedState":      baseline,
		"externalSecretState": fixtureBaseline,
	})
}

func captureGenerated(ctx context.Context, environment testkit.CaseContext) (generatedSnapshot, map[string][]byte, map[string][]byte, error) {
	objects, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	internal, err := support.FindObject(objects, "Secret", "internal-initializer")
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	admin, err := support.FindObject(objects, "Secret", "admin-initializer")
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	internalRecord, err := support.FindObject(objects, "ConfigMap", "internal-initializer")
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	adminRecord, err := support.FindObject(objects, "ConfigMap", "admin-initializer")
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	internalObject, internalData, err := support.GetSecret(ctx, environment.Namespace, internal.Name)
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	adminObject, adminData, err := support.GetSecret(ctx, environment.Namespace, admin.Name)
	if err != nil {
		return generatedSnapshot{}, nil, nil, err
	}
	snapshot := generatedSnapshot{
		Internal: secretSnapshot{Name: internalObject.Name, UID: internalObject.UID, Hashes: support.SecretHashes(internalData)},
		Admin:    secretSnapshot{Name: adminObject.Name, UID: adminObject.UID, Hashes: support.SecretHashes(adminData)},
		InternalRecord: recordSnapshot{Name: internalRecord.Name, UID: internalRecord.UID,
			SHA256: hashBytes([]byte(testkit.String(internalRecord.Data["record.json"])))},
		AdminRecord: recordSnapshot{Name: adminRecord.Name, UID: adminRecord.UID,
			SHA256: hashBytes([]byte(testkit.String(adminRecord.Data["record.json"])))},
	}
	return snapshot, internalData, adminData, nil
}

func createFixtures(ctx context.Context, environment testkit.CaseContext, manifestDir string, internalData, adminData map[string][]byte) ([]fixture, error) {
	definitions := []fixture{
		{Name: fixtureName(environment, "admin"), Data: selectKeys(adminData, "config.json")},
		{Name: fixtureName(environment, "auth"), Data: selectKeys(internalData, "config.json")},
		{Name: fixtureName(environment, "htpasswd"), Data: selectKeys(internalData, "htpasswd")},
		{Name: fixtureName(environment, "tls"), Data: selectKeys(internalData, "ca.crt", "tls.crt", "tls.key")},
		{Name: fixtureName(environment, "http"), Data: selectKeys(internalData, "http-secret")},
	}
	for index := range definitions {
		object, err := createOrVerifyFixture(ctx, environment, manifestDir, definitions[index].Name, definitions[index].Data)
		if err != nil {
			return nil, err
		}
		definitions[index].Object = secretSnapshot{Name: object.Name, UID: object.UID, Hashes: support.SecretHashes(definitions[index].Data)}
	}
	return definitions, nil
}
