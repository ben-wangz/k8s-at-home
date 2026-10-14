package adminauth

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "admin-auth")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Fullname+"-admin-auth")
	if err != nil {
		return err
	}
	if len(data) != 1 || len(data["config.json"]) == 0 {
		return fmt.Errorf("management auth Secret must contain only config.json")
	}
	var configuration support.AdminAuth
	if err := json.Unmarshal(data["config.json"], &configuration); err != nil {
		return fmt.Errorf("management auth configuration is invalid")
	}
	if len(configuration.Basic) != 1 || len(configuration.Bearer) != 0 {
		return fmt.Errorf("management auth must contain exactly one Basic credential")
	}
	credential := configuration.Basic[0]
	if credential.Username != "admin" || len(credential.Password) != 64 || credential.Token != "" {
		return fmt.Errorf("management auth credential is invalid")
	}
	if _, err := hex.DecodeString(credential.Password); err != nil {
		return fmt.Errorf("management password is not hexadecimal")
	}
	expectedPermissions := []string{"gc-admin", "referrer-prune", "task-admin"}
	actualPermissions := append([]string(nil), credential.Permissions...)
	sort.Strings(actualPermissions)
	if !equalStrings(actualPermissions, expectedPermissions) {
		return fmt.Errorf("management auth permissions do not match the required set")
	}
	record, err := findAdminRecord(ctx, environment)
	if err != nil {
		return err
	}
	state, err := support.DecodeRecord(record)
	if err != nil {
		return err
	}
	if state.Protocol != "admin-v1" || state.Instance != environment.Fullname || state.SecretUID != secret.UID {
		return fmt.Errorf("management initialization record does not match the Secret UID")
	}
	annotations := testkit.Map(secret.Metadata["annotations"])
	if secret.Labels["registry-hub.io/managed-by"] != "admin-initializer" ||
		testkit.String(annotations["registry-hub.io/admin-scope"]) == "" {
		return fmt.Errorf("management auth Secret ownership metadata is incomplete")
	}
	if err := verifyDocsAuthentication(ctx, environment, credential); err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"secretUID": secret.UID,
		"keySHA256": support.SecretHashes(data),
		"recordUID": record.UID,
		"docsAuth":  "verified",
	})
}

func findAdminRecord(ctx context.Context, environment testkit.CaseContext) (testkit.Object, error) {
	objects, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return testkit.Object{}, err
	}
	return support.FindObject(objects, "ConfigMap", "admin-initializer")
}

func verifyDocsAuthentication(ctx context.Context, environment testkit.CaseContext, credential support.AdminCredential) error {
	client := testkit.NewHTTPClient(10*time.Second, nil)
	for _, path := range []string{"/docs", "/openapi.yaml"} {
		endpoint := environment.AdminURL + path
		response, err := testkit.Request(ctx, client, http.MethodGet, endpoint, nil, nil, nil)
		if err != nil {
			return err
		}
		if err := testkit.ExpectStatus(response, http.StatusUnauthorized); err != nil {
			return fmt.Errorf("management endpoint %s is not protected", path)
		}
		response, err = testkit.Request(ctx, client, http.MethodGet, endpoint, nil, nil, &testkit.BasicCredential{
			Username: credential.Username, Password: credential.Password,
		})
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("management endpoint %s rejected the generated credential", path)
		}
		if _, err := testkit.ReadBody(response, 2<<20); err != nil {
			return err
		}
	}
	response, err := testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+"/docs", nil, nil, &testkit.BasicCredential{
		Username: "admin", Password: "invalid-registry-hub-test-credential",
	})
	if err != nil {
		return err
	}
	if err := testkit.ExpectStatus(response, http.StatusUnauthorized); err != nil {
		return fmt.Errorf("management docs accepted an invalid credential")
	}
	return nil
}

func equalStrings(left, right []string) bool {
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
