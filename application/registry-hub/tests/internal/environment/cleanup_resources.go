package environment

import (
	"context"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func canonicalResourceKind(kind string) string {
	switch kind {
	case "Job":
		return "job"
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
	for _, suffix := range []string{"admin", "auth", "htpasswd", "tls", "http", "client-auth", "history-admin", "upstream-auth", "upstream-proxy", "backend-s3", "registry-s3", "registry-s3-tls", "referrers-probe", "referrers-migration-config", "referrers-migration-registry", "quota-seed"} {
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
	if identity.Kind != "secret" && identity.Kind != "configmap" && identity.Kind != "service" && identity.Kind != "pod" && identity.Kind != "job" {
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
	if testkit.String(metadata["uid"]) != identity.UID {
		return fmt.Errorf("generated object %s ownership changed; refusing deletion", identity.Name)
	}
	if identity.Kind == "job" {
		if err := validateGCJobOwnership(object, fullname); err != nil {
			return fmt.Errorf("generated job %s ownership changed; refusing deletion: %w", identity.Name, err)
		}
		_, err = testkit.Command(ctx, "kubectl", "-n", Namespace, "delete", "job", identity.Name,
			"--preconditions=uid="+identity.UID, "--cascade=foreground", "--wait=true", "--timeout=60s")
		return err
	}
	if testkit.String(labels["registry-hub.io/instance"]) != fullname {
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
