package rbacboundary

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "rbac-boundary")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	generated, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return err
	}
	internalSecret, err := support.FindObject(generated, "Secret", "internal-initializer")
	if err != nil {
		return err
	}
	adminSecret, err := support.FindObject(generated, "Secret", "admin-initializer")
	if err != nil {
		return err
	}
	internalRecord, err := support.FindObject(generated, "ConfigMap", "internal-initializer")
	if err != nil {
		return err
	}
	adminRecord, err := support.FindObject(generated, "ConfigMap", "admin-initializer")
	if err != nil {
		return err
	}

	roleName := environment.Fullname + "-registry-init"
	role, err := testkit.KubectlGet(ctx, environment.Namespace, "role", roleName)
	if err != nil {
		return err
	}
	if err := verifyRole(role, environment.Namespace, []string{internalSecret.Name, adminSecret.Name}, []string{internalRecord.Name, adminRecord.Name}); err != nil {
		return err
	}
	binding, err := testkit.KubectlGet(ctx, environment.Namespace, "rolebinding", roleName)
	if err != nil {
		return err
	}
	if err := verifyBinding(binding, roleName, environment.Fullname+"-registry-init", environment.Namespace); err != nil {
		return err
	}

	jobList, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", "app.kubernetes.io/instance="+environment.Release+",app.kubernetes.io/component=internal-initialization")
	if err != nil {
		return err
	}
	if len(jobList) != 1 {
		return fmt.Errorf("expected one initialization Job, found %d", len(jobList))
	}
	jobSpec := testkit.Map(testkit.Map(testkit.Map(jobList[0].Raw["spec"])["template"])["spec"])
	initServiceAccount := testkit.String(jobSpec["serviceAccountName"])
	if initServiceAccount != environment.Fullname+"-registry-init" || jobSpec["automountServiceAccountToken"] != true {
		return fmt.Errorf("initialization Job does not use its dedicated ServiceAccount")
	}

	backend, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	backendSpec := deploymentPodSpec(backend)
	backendServiceAccount := testkit.String(backendSpec["serviceAccountName"])
	if backendServiceAccount == "" || backendServiceAccount == initServiceAccount {
		return fmt.Errorf("backend uses the initialization ServiceAccount")
	}
	if err := verifyNoBackendRoleBinding(ctx, environment, backendServiceAccount); err != nil {
		return err
	}
	registry, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	registrySpec := deploymentPodSpec(registry)
	if registrySpec["automountServiceAccountToken"] != false {
		return fmt.Errorf("Registry workload mounts a ServiceAccount token")
	}

	if err := support.WriteResult(environment, map[string]any{
		"role":                  roleName,
		"roleBinding":           roleName,
		"secretNames":           sorted([]string{internalSecret.Name, adminSecret.Name}),
		"recordNames":           sorted([]string{internalRecord.Name, adminRecord.Name}),
		"initServiceAccount":    initServiceAccount,
		"backendServiceAccount": backendServiceAccount,
		"registryTokenMounted":  false,
	}); err != nil {
		return err
	}
	return nil
}

func verifyRole(role map[string]any, namespace string, secretNames, recordNames []string) error {
	if testkit.String(testkit.Map(role["metadata"])["namespace"]) != namespace {
		return fmt.Errorf("initialization Role is not namespace scoped to the test environment")
	}
	rules := testkit.Slice(role["rules"])
	if len(rules) != 4 {
		return fmt.Errorf("initialization Role has unexpected rule count")
	}
	if !hasRule(rules, "secrets", []string{"get"}, secretNames) ||
		!hasRule(rules, "secrets", []string{"create"}, nil) ||
		!hasRule(rules, "configmaps", []string{"get", "update"}, recordNames) ||
		!hasRule(rules, "configmaps", []string{"create"}, nil) {
		return fmt.Errorf("initialization Role grants permissions outside the expected target operations")
	}
	return nil
}

func hasRule(rules []any, resource string, verbs, names []string) bool {
	for _, raw := range rules {
		rule := testkit.Map(raw)
		if !equalStrings(stringValues(rule["apiGroups"]), []string{""}) ||
			!equalStrings(stringValues(rule["resources"]), []string{resource}) ||
			!equalStrings(stringValues(rule["verbs"]), verbs) ||
			len(testkit.Slice(rule["nonResourceURLs"])) != 0 {
			continue
		}
		actualNames := stringValues(rule["resourceNames"])
		if equalStrings(actualNames, names) {
			return true
		}
	}
	return false
}

func verifyBinding(binding map[string]any, roleName, serviceAccount, namespace string) error {
	if testkit.String(testkit.Map(binding["metadata"])["namespace"]) != namespace {
		return fmt.Errorf("initialization RoleBinding is not namespace scoped to the test environment")
	}
	roleRef := testkit.Map(binding["roleRef"])
	if testkit.String(roleRef["apiGroup"]) != "rbac.authorization.k8s.io" || testkit.String(roleRef["kind"]) != "Role" || testkit.String(roleRef["name"]) != roleName {
		return fmt.Errorf("initialization RoleBinding does not reference the namespace Role")
	}
	subjects := testkit.Slice(binding["subjects"])
	if len(subjects) != 1 {
		return fmt.Errorf("initialization RoleBinding has unexpected subjects")
	}
	subject := testkit.Map(subjects[0])
	if testkit.String(subject["kind"]) != "ServiceAccount" || testkit.String(subject["name"]) != serviceAccount || testkit.String(subject["namespace"]) != namespace {
		return fmt.Errorf("initialization RoleBinding targets an unexpected identity")
	}
	return nil
}

func verifyNoBackendRoleBinding(ctx context.Context, environment testkit.CaseContext, serviceAccount string) error {
	bindings, err := testkit.KubectlList(ctx, environment.Namespace, "get", "rolebindings", "-l", "app.kubernetes.io/instance="+environment.Release)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		for _, raw := range testkit.Slice(binding.Raw["subjects"]) {
			subject := testkit.Map(raw)
			if testkit.String(subject["kind"]) == "ServiceAccount" &&
				testkit.String(subject["name"]) == serviceAccount &&
				testkit.String(subject["namespace"]) == environment.Namespace {
				return fmt.Errorf("backend ServiceAccount is granted chart-owned namespace RBAC")
			}
		}
	}
	return nil
}

func deploymentPodSpec(deployment map[string]any) map[string]any {
	spec := testkit.Map(deployment["spec"])
	template := testkit.Map(spec["template"])
	return testkit.Map(testkit.Map(template["spec"]))
}

func stringValues(value any) []string {
	items := testkit.Slice(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, testkit.String(item))
	}
	return result
}

func equalStrings(left, right []string) bool {
	left = sorted(left)
	right = sorted(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sorted(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
