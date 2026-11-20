package environment

import (
	"context"
	"fmt"

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
	gcJobs, err := environment.gcJobsForInstance(ctx, fullname)
	if err != nil {
		return nil, nil, err
	}
	generated = append(generated, gcJobs...)
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
	if err := environment.deleteGCJobsForInstance(ctx, registration.ID, registration.Fullname); err != nil {
		return nil, err
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
