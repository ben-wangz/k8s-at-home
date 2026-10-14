package support

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (registry *RegistryClient) Referrers(ctx context.Context, repository, subject string, query url.Values) (*http.Response, error) {
	endpoint := registry.Endpoint(repository, "/referrers/") + subject
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	return registry.Request(ctx, http.MethodGet, endpoint, nil, nil)
}

func AddQuery(endpoint, key, value string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Add(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func VerifyResponseBody(response *http.Response, expected []byte, limit int64) error {
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("expected 200, got %s", response.Status)
	}
	actual, err := testkit.ReadBody(response, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) || Digest(actual) != Digest(expected) {
		return fmt.Errorf("response body did not match expected bytes")
	}
	return nil
}

func DecodeJSON(response *http.Response, limit int64, target any) ([]byte, error) {
	body, err := testkit.ReadBody(response, limit)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, err
	}
	return body, nil
}

func SortedTags(tags []string) []string {
	result := append([]string(nil), tags...)
	sort.Strings(result)
	return result
}

func ReadAdminCredential(ctx context.Context, environment testkit.CaseContext) (testkit.BasicCredential, error) {
	secret, data, err := GetSecret(ctx, environment.Namespace, environment.Fullname+"-admin-auth")
	if err != nil {
		return testkit.BasicCredential{}, err
	}
	if secret.UID == "" {
		return testkit.BasicCredential{}, fmt.Errorf("admin Secret has no UID")
	}
	var configuration AdminAuth
	if err := json.Unmarshal(data["config.json"], &configuration); err != nil {
		return testkit.BasicCredential{}, err
	}
	if len(configuration.Basic) != 1 {
		return testkit.BasicCredential{}, fmt.Errorf("admin Secret has no single Basic credential")
	}
	return testkit.BasicCredential{Username: configuration.Basic[0].Username, Password: configuration.Basic[0].Password}, nil
}

func AdminRequest(ctx context.Context, environment testkit.CaseContext, method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	credential, err := ReadAdminCredential(ctx, environment)
	if err != nil {
		return nil, err
	}
	return testkit.Request(ctx, testkit.NewHTTPClient(30*time.Second, nil), method, strings.TrimRight(environment.AdminURL, "/")+path, body, headers, &credential)
}
