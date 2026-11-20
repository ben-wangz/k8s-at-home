package environment

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (environment Environment) gcJobsForInstance(ctx context.Context, fullname string) ([]testkit.ResourceIdentity, error) {
	objects, err := testkit.KubectlJSON(ctx, Namespace, "get", "jobs", "-l", "app.kubernetes.io/name=registry-hub,app.kubernetes.io/component=gc")
	if err != nil {
		return nil, err
	}
	result := make([]testkit.ResourceIdentity, 0)
	for _, raw := range testkit.Slice(objects["items"]) {
		object := testkit.Map(raw)
		identity, owned, err := gcJobIdentity(object, fullname)
		if err != nil {
			return nil, err
		}
		if owned {
			result = append(result, identity)
		}
	}
	return result, nil
}

func (environment Environment) deleteGCJobsForInstance(ctx context.Context, runID, fullname string) error {
	jobs, err := environment.gcJobsForInstance(ctx, fullname)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := deleteGeneratedObject(ctx, runID, fullname, job); err != nil {
			return err
		}
	}
	remaining, err := environment.gcJobsForInstance(ctx, fullname)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("test instance %s still owns GC job %s", fullname, remaining[0].Name)
	}
	return nil
}

func gcJobIdentity(object map[string]any, fullname string) (testkit.ResourceIdentity, bool, error) {
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	spec := testkit.Map(object["spec"])
	template := testkit.Map(spec["template"])
	podSpec := testkit.Map(template["spec"])
	if testkit.String(podSpec["serviceAccountName"]) != fullname {
		return testkit.ResourceIdentity{}, false, nil
	}
	if testkit.String(labels["app.kubernetes.io/name"]) != "registry-hub" ||
		testkit.String(labels["app.kubernetes.io/component"]) != "gc" {
		return testkit.ResourceIdentity{}, false, fmt.Errorf("job using test service account %s has unexpected labels", fullname)
	}
	name := testkit.String(metadata["name"])
	gcID := testkit.String(labels["registry-hub.io/gc-id"])
	if gcID == "" || name != "registry-hub-gc-"+gcID {
		return testkit.ResourceIdentity{}, false, fmt.Errorf("job using test service account %s has an invalid GC identity", fullname)
	}
	identity, err := resourceIdentity(object)
	if err != nil {
		return testkit.ResourceIdentity{}, false, err
	}
	return identity, true, nil
}

func validateGCJobOwnership(object map[string]any, fullname string) error {
	_, owned, err := gcJobIdentity(object, fullname)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("job does not reference test service account %s", fullname)
	}
	return nil
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
