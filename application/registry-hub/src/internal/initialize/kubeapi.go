package initialize

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type kubeClient struct {
	baseURL   string
	tokenFile string
	client    *http.Client
}

type objectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

type secretObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMeta        `json:"metadata"`
	Immutable  bool              `json:"immutable,omitempty"`
	Type       string            `json:"type"`
	Data       map[string][]byte `json:"data"`
}

type configMapObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMeta        `json:"metadata"`
	Data       map[string]string `json:"data"`
}

type apiStatus struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type apiError struct {
	Status int
	Reason string
}

func (e *apiError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("Kubernetes API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("Kubernetes API returned HTTP %d (%s)", e.Status, e.Reason)
}

func newKubeClient() (*kubeClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes service address is unavailable")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, errors.New("Kubernetes service port is invalid")
	}
	serviceAccount := os.Getenv("KUBERNETES_SERVICE_ACCOUNT_DIR")
	if serviceAccount == "" {
		serviceAccount = defaultServiceAccount
	}
	caPEM, err := os.ReadFile(serviceAccount + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes CA contains no certificates")
	}
	transport := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	return &kubeClient{
		baseURL:   "https://" + net.JoinHostPort(host, port),
		tokenFile: serviceAccount + "/token",
		client:    &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}, nil
}

func (c *kubeClient) request(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, fmt.Errorf("encode Kubernetes request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return 0, fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("create Kubernetes request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("Kubernetes API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var status apiStatus
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status)
		return response.StatusCode, &apiError{Status: response.StatusCode, Reason: status.Reason}
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
			return response.StatusCode, fmt.Errorf("decode Kubernetes response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func (c *kubeClient) objectPath(namespace, resource, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/" + resource + "/" + url.PathEscape(name)
}

func isNotFound(err error) bool {
	var status *apiError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

func isAlreadyExists(err error) bool {
	var status *apiError
	return errors.As(err, &status) && status.Status == http.StatusConflict
}

func isConflict(err error) bool {
	var status *apiError
	return errors.As(err, &status) && status.Status == http.StatusConflict
}

func (c *kubeClient) getSecret(ctx context.Context, namespace, name string) (*secretObject, error) {
	var object secretObject
	_, err := c.request(ctx, http.MethodGet, c.objectPath(namespace, "secrets", name), nil, &object)
	if err != nil {
		return nil, err
	}
	return &object, nil
}

func (c *kubeClient) createSecret(ctx context.Context, object *secretObject) error {
	_, err := c.request(ctx, http.MethodPost, "/api/v1/namespaces/"+object.Metadata.Namespace+"/secrets", object, nil)
	return err
}

func (c *kubeClient) getConfigMap(ctx context.Context, namespace, name string) (*configMapObject, error) {
	var object configMapObject
	_, err := c.request(ctx, http.MethodGet, c.objectPath(namespace, "configmaps", name), nil, &object)
	if err != nil {
		return nil, err
	}
	return &object, nil
}

func (c *kubeClient) createConfigMap(ctx context.Context, object *configMapObject) error {
	_, err := c.request(ctx, http.MethodPost, "/api/v1/namespaces/"+object.Metadata.Namespace+"/configmaps", object, nil)
	return err
}

func (c *kubeClient) updateConfigMap(ctx context.Context, object *configMapObject) error {
	_, err := c.request(ctx, http.MethodPut, c.objectPath(object.Metadata.Namespace, "configmaps", object.Metadata.Name), object, nil)
	return err
}
