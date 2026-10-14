package environment

import (
	"context"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (environment Environment) OwnedResources(ctx context.Context, id, release, fullname string) ([]testkit.ResourceIdentity, []testkit.ResourceIdentity, error) {
	pvcs, err := environment.listIdentities(ctx, "persistentvolumeclaims", "app.kubernetes.io/instance="+release)
	if err != nil {
		return nil, nil, err
	}
	generated := make([]testkit.ResourceIdentity, 0)
	for _, managedBy := range []string{"internal-initializer", "admin-initializer"} {
		selector := "registry-hub.io/instance=" + fullname + ",registry-hub.io/managed-by=" + managedBy
		objects, err := testkit.KubectlJSON(ctx, Namespace, "get", "secrets,configmaps", "-l", selector)
		if err != nil {
			return nil, nil, err
		}
		for _, raw := range testkit.Slice(objects["items"]) {
			object := testkit.Map(raw)
			labels := testkit.Map(testkit.Map(object["metadata"])["labels"])
			if testkit.String(labels["registry-hub.io/instance"]) != fullname || testkit.String(labels["registry-hub.io/managed-by"]) != managedBy {
				return nil, nil, fmt.Errorf("generated object has unexpected instance identity")
			}
			identity, err := resourceIdentity(object)
			if err != nil {
				return nil, nil, err
			}
			generated = append(generated, identity)
		}
	}
	selector := "registry-hub.io/test-id=" + id + ",registry-hub.io/instance=" + fullname + ",registry-hub.io/managed-by=registry-hub-test-fixture"
	fixtures, err := testkit.KubectlJSON(ctx, Namespace, "get", "secrets,configmaps,services,pods", "-l", selector)
	if err != nil {
		return nil, nil, err
	}
	for _, raw := range testkit.Slice(fixtures["items"]) {
		object := testkit.Map(raw)
		metadata := testkit.Map(object["metadata"])
		labels := testkit.Map(metadata["labels"])
		name := testkit.String(metadata["name"])
		if testkit.String(labels["registry-hub.io/test-id"]) != id ||
			testkit.String(labels["registry-hub.io/instance"]) != fullname ||
			testkit.String(labels["registry-hub.io/managed-by"]) != "registry-hub-test-fixture" ||
			!knownFixtureName(release, name) {
			return nil, nil, fmt.Errorf("test fixture %s has unexpected ownership", name)
		}
		identity, err := resourceIdentity(object)
		if err != nil {
			return nil, nil, err
		}
		generated = append(generated, identity)
	}
	return pvcs, generated, nil
}

func (environment Environment) Destroy(ctx context.Context, registration Registration, registrationUID string) ([]testkit.ResourceIdentity, error) {
	pvcs, generated, err := environment.OwnedResources(ctx, registration.ID, registration.Release, registration.Fullname)
	if err != nil {
		return nil, err
	}
	if err := environment.Uninstall(ctx, registration.Release); err != nil {
		return nil, err
	}
	for _, resource := range pvcs {
		if err := deleteOwnedPVC(ctx, registration.Release, resource); err != nil {
			return nil, err
		}
	}
	for _, resource := range generated {
		if err := deleteGeneratedObject(ctx, registration.ID, registration.Fullname, resource); err != nil {
			return nil, err
		}
	}
	if err := environment.ensureReleaseResourcesGone(ctx, registration.Release); err != nil {
		return nil, err
	}
	if err := environment.DeleteRegistration(ctx, registration.ID, registrationUID); err != nil {
		return nil, err
	}
	return append(pvcs, generated...), nil
}

func (environment Environment) VerifyAbsent(ctx context.Context, id string) error {
	for _, scenarioID := range ScenarioIDs() {
		release, err := ReleaseName(scenarioID, id)
		if err != nil {
			return err
		}
		fullname := release + "-registry-hub"
		exists, err := environment.ReleaseExists(ctx, release)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("Helm release %s remains without its test registration", release)
		}
		for _, resource := range []string{
			"pods", "deployments", "services", "jobs", "persistentvolumeclaims", "configmaps",
			"secrets", "roles", "rolebindings", "serviceaccounts", "networkpolicies",
		} {
			remaining, err := environment.listIdentities(ctx, resource, "app.kubernetes.io/instance="+release)
			if err != nil {
				return err
			}
			if len(remaining) > 0 {
				return fmt.Errorf("run %s still owns %s %s without its test registration", id, resource, remaining[0].Name)
			}
		}
		_, generated, err := environment.OwnedResources(ctx, id, release, fullname)
		if err != nil {
			return err
		}
		if len(generated) > 0 {
			return fmt.Errorf("run %s still owns generated %s %s without its test registration", id, generated[0].Kind, generated[0].Name)
		}
	}
	return nil
}

func (environment Environment) listIdentities(ctx context.Context, resource, selector string) ([]testkit.ResourceIdentity, error) {
	objects, err := testkit.KubectlJSON(ctx, Namespace, "get", resource, "-l", selector)
	if err != nil {
		return nil, err
	}
	items := testkit.Slice(objects["items"])
	result := make([]testkit.ResourceIdentity, 0, len(items))
	for _, raw := range items {
		object := testkit.Map(raw)
		metadata := testkit.Map(object["metadata"])
		if testkit.String(metadata["name"]) == "" || testkit.String(metadata["uid"]) == "" {
			return nil, fmt.Errorf("resource identity for %s is incomplete", resource)
		}
		result = append(result, testkit.ResourceIdentity{
			Kind: resource,
			Name: testkit.String(metadata["name"]),
			UID:  testkit.String(metadata["uid"]),
		})
	}
	return result, nil
}

func canonicalResourceKind(kind string) string {
	switch kind {
	case "Secret":
		return "secret"
	case "ConfigMap":
		return "configmap"
	case "Pod":
		return "pod"
	case "Service":
		return "service"
	default:
		return ""
	}
}

func resourceIdentity(object map[string]any) (testkit.ResourceIdentity, error) {
	metadata := testkit.Map(object["metadata"])
	identity := testkit.ResourceIdentity{Name: testkit.String(metadata["name"]), UID: testkit.String(metadata["uid"])}
	if identity.Name == "" || identity.UID == "" {
		return testkit.ResourceIdentity{}, fmt.Errorf("resource identity is incomplete")
	}
	identity.Kind = canonicalResourceKind(testkit.String(object["kind"]))
	if identity.Kind == "" {
		return testkit.ResourceIdentity{}, fmt.Errorf("resource has an unsupported kind")
	}
	return identity, nil
}

func knownFixtureName(release, name string) bool {
	for _, suffix := range []string{"admin", "auth", "htpasswd", "tls", "http", "client-auth", "upstream-auth", "upstream-proxy", "backend-s3", "registry-s3", "registry-s3-tls", "referrers-probe", "referrers-migration-config", "referrers-migration-registry"} {
		if name == release+"-external-"+suffix {
			return true
		}
	}
	return false
}

func deleteOwnedPVC(ctx context.Context, release string, identity testkit.ResourceIdentity) error {
	object, err := testkit.KubectlGet(ctx, Namespace, "persistentvolumeclaim", identity.Name)
	if err != nil {
		if testkit.IsNotFound(err) {
			return nil
		}
		return err
	}
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["uid"]) != identity.UID || testkit.String(labels["app.kubernetes.io/instance"]) != release {
		return fmt.Errorf("PVC %s ownership changed; refusing deletion", identity.Name)
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", Namespace, "delete", "pvc", identity.Name, "--wait=true", "--timeout=5m")
	return err
}

func deleteGeneratedObject(ctx context.Context, runID, fullname string, identity testkit.ResourceIdentity) error {
	if identity.Kind != "secret" && identity.Kind != "configmap" && identity.Kind != "service" && identity.Kind != "pod" {
		return fmt.Errorf("generated object %s has an unsupported kind", identity.Name)
	}
	if identity.Name == "" || identity.UID == "" {
		return fmt.Errorf("generated object identity is incomplete")
	}
	object, err := testkit.KubectlGet(ctx, Namespace, identity.Kind, identity.Name)
	if err != nil {
		if testkit.IsNotFound(err) {
			return nil
		}
		return err
	}
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["uid"]) != identity.UID ||
		testkit.String(labels["registry-hub.io/instance"]) != fullname {
		return fmt.Errorf("generated object %s ownership changed; refusing deletion", identity.Name)
	}
	managedBy := testkit.String(labels["registry-hub.io/managed-by"])
	if managedBy == "registry-hub-test-fixture" {
		if testkit.String(labels["registry-hub.io/test-id"]) != runID || !knownFixtureName(strings.TrimSuffix(fullname, "-registry-hub"), identity.Name) {
			return fmt.Errorf("test fixture %s ownership changed; refusing deletion", identity.Name)
		}
	} else if managedBy != "internal-initializer" && managedBy != "admin-initializer" {
		return fmt.Errorf("generated object %s has unknown managed-by label", identity.Name)
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", Namespace, "delete", identity.Kind, identity.Name, "--wait=true", "--timeout=60s")
	return err
}

func (environment Environment) ensureReleaseResourcesGone(ctx context.Context, release string) error {
	for _, resource := range []string{"pods", "deployments", "services", "jobs", "persistentvolumeclaims", "configmaps", "secrets", "roles", "rolebindings", "serviceaccounts", "networkpolicies"} {
		remaining, err := environment.listIdentities(ctx, resource, "app.kubernetes.io/instance="+release)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return fmt.Errorf("release %s still owns %s %s", release, resource, remaining[0].Name)
		}
	}
	return nil
}
