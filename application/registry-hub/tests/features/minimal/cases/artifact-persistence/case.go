package artifactpersistence

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/http"
	"sort"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

type objectSnapshot struct {
	UID    string
	Hashes map[string]string
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "artifact-persistence")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	artifact := environment.DependencyResults["artifact-roundtrip"]
	repository, err := support.DetailString(artifact, "repository")
	if err != nil {
		return err
	}
	subject, err := support.DetailString(artifact, "subjectDigest")
	if err != nil {
		return err
	}
	referrer := environment.DependencyResults["referrers-read"]
	task := environment.DependencyResults["admin-api"]
	if task.CaseID == "" {
		return fmt.Errorf("security task dependency is unavailable")
	}
	tagRepository, err := support.DetailString(environment.DependencyResults["tag-publication"], "repository")
	if err != nil {
		return err
	}
	taskID, err := support.DetailString(task, "taskID")
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	beforeManifest, _, err := support.ReadManifest(ctx, registry, repository, subject, support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	beforeReferrers, err := readReferrers(ctx, registry, repository, subject)
	if err != nil {
		return err
	}
	beforeObjects, err := captureObjects(ctx, environment)
	if err != nil {
		return err
	}
	if err := restartDeployments(ctx, environment); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "helm", "upgrade", "--install", environment.Release, environment.ChartDir, "--namespace", environment.Namespace, "--values", environment.BaseValues, "--values", environment.ValuesFile, "--wait", "--wait-for-jobs", "--timeout", "15m"); err != nil {
		return err
	}
	afterObjects, err := captureObjects(ctx, environment)
	if err != nil {
		return err
	}
	if err := compareObjects(beforeObjects, afterObjects); err != nil {
		return err
	}
	afterManifest, _, err := support.ReadManifest(ctx, registry, repository, subject, support.OCIManifestMediaType)
	if err != nil || !bytes.Equal(beforeManifest, afterManifest) {
		return fmt.Errorf("manifest bytes changed across restart or upgrade")
	}
	afterReferrers, err := readReferrers(ctx, registry, repository, subject)
	if err != nil || !bytes.Equal(beforeReferrers, afterReferrers) {
		return fmt.Errorf("referrer index changed across restart or upgrade")
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+taskID, nil, nil)
	if err != nil {
		return err
	}
	var taskState map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &taskState); err != nil {
		return err
	}
	if testkit.String(taskState["id"]) != taskID || testkit.String(taskState["status"]) != "failed" {
		return fmt.Errorf("failure task changed state across restart")
	}
	return support.WriteResult(environment, map[string]any{
		"repository": repository, "manifestDigest": subject, "referrerIndexSHA256": support.Digest(afterReferrers),
		"taskID": taskID, "taskStatus": testkit.String(taskState["status"]), "objectUIDs": objectUIDs(afterObjects),
		"tagRepository": tagRepository,
		"referrerCase":  referrer.CaseID,
	})
}

func restartDeployments(ctx context.Context, environment testkit.CaseContext) error {
	for _, name := range []string{environment.Fullname, environment.Fullname + "-registry"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+name); err != nil {
			return err
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+name, "--timeout=5m"); err != nil {
			return err
		}
	}
	return nil
}

func captureObjects(ctx context.Context, environment testkit.CaseContext) (map[string]objectSnapshot, error) {
	result := make(map[string]objectSnapshot)
	for key, name := range map[string]string{"internal": environment.Fullname + "-registry-internal", "admin": environment.Fullname + "-admin-auth"} {
		object, data, err := support.GetSecret(ctx, environment.Namespace, name)
		if err != nil {
			return nil, err
		}
		result[key] = objectSnapshot{UID: object.UID, Hashes: support.SecretHashes(data)}
	}
	generated, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return nil, err
	}
	for _, managedBy := range []string{"internal-initializer", "admin-initializer"} {
		object, findErr := support.FindObject(generated, "ConfigMap", managedBy)
		if findErr != nil {
			return nil, findErr
		}
		result[managedBy] = objectSnapshot{UID: object.UID, Hashes: map[string]string{"record": support.Digest([]byte(fmt.Sprint(object.Data)))}}
	}
	objects, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pvc", "-l", "app.kubernetes.io/instance="+environment.Release)
	if err != nil {
		return nil, err
	}
	for _, object := range objects {
		result["pvc/"+object.Name] = objectSnapshot{UID: object.UID}
	}
	return result, nil
}

func compareObjects(before, after map[string]objectSnapshot) error {
	if len(before) != len(after) {
		return fmt.Errorf("generated object set changed across restart")
	}
	for key, left := range before {
		right, ok := after[key]
		if !ok || left.UID != right.UID || len(left.Hashes) != len(right.Hashes) {
			return fmt.Errorf("object %s identity changed across restart", key)
		}
		for hashKey, hash := range left.Hashes {
			if right.Hashes[hashKey] != hash {
				return fmt.Errorf("object %s data changed across restart", key)
			}
		}
	}
	return nil
}

func readReferrers(ctx context.Context, registry *support.RegistryClient, repository, subject string) ([]byte, error) {
	response, err := registry.Referrers(ctx, repository, subject, nil)
	if err != nil {
		return nil, err
	}
	return testkit.ReadBody(response, 4<<20)
}

func objectUIDs(objects map[string]objectSnapshot) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+":"+objects[key].UID)
	}
	return result
}
