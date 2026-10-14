package environment

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const Namespace = "develop"

type Environment struct {
	Root     string
	AppDir   string
	ChartDir string
	TestsDir string
}

type PreflightResult struct {
	KubeContext    string   `json:"kubeContext"`
	AvailableTools []string `json:"availableTools"`
}

func New(root string) (Environment, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Environment{}, err
	}
	appDir := filepath.Join(absoluteRoot, "application", "registry-hub")
	for _, path := range []string{filepath.Join(appDir, "chart", "Chart.yaml"), filepath.Join(appDir, "container", "Containerfile"), filepath.Join(appDir, "tests", "go.mod")} {
		if _, err := os.Stat(path); err != nil {
			return Environment{}, fmt.Errorf("required project path %s is unavailable: %w", path, err)
		}
	}
	return Environment{Root: absoluteRoot, AppDir: appDir, ChartDir: filepath.Join(appDir, "chart"), TestsDir: filepath.Join(appDir, "tests")}, nil
}

func (environment Environment) CurrentContext(ctx context.Context) (string, error) {
	result, err := testkit.Command(ctx, "kubectl", "config", "current-context")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(result.Stdout))
	if name == "" {
		return "", fmt.Errorf("kubectl has no current context")
	}
	return name, nil
}

func (environment Environment) Check(ctx context.Context, needsPodman bool) (PreflightResult, error) {
	required := []string{"go", "helm", "kubectl"}
	if needsPodman {
		required = append(required, "podman")
	}
	for _, name := range required {
		if _, err := exec.LookPath(name); err != nil {
			return PreflightResult{}, fmt.Errorf("required command %q was not found", name)
		}
	}
	contextName, err := environment.CurrentContext(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	if _, err := testkit.Command(ctx, "kubectl", "get", "namespace", Namespace, "-o", "json"); err != nil {
		return PreflightResult{}, fmt.Errorf("namespace %q is not readable: %w", Namespace, err)
	}
	for _, resource := range []string{"deployments", "services", "jobs", "persistentvolumeclaims", "secrets", "configmaps", "serviceaccounts", "roles", "rolebindings", "networkpolicies"} {
		for _, verb := range []string{"create", "get", "list", "watch", "patch", "delete"} {
			if err := environment.requireCan(ctx, verb, resource); err != nil {
				return PreflightResult{}, err
			}
		}
	}
	for _, permission := range [][2]string{{"create", "pods"}, {"delete", "pods"}, {"list", "pods"}, {"get", "pods"}, {"get", "pods/log"}, {"get", "endpoints"}, {"list", "endpoints"}, {"watch", "endpoints"}, {"get", "resourcequotas"}, {"get", "limitranges"}, {"update", "configmaps"}} {
		if err := environment.requireCan(ctx, permission[0], permission[1]); err != nil {
			return PreflightResult{}, err
		}
	}
	for _, resource := range []string{"resourcequotas", "limitranges"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "get", resource, "-o", "json"); err != nil {
			return PreflightResult{}, fmt.Errorf("cannot inspect namespace %s %s: %w", Namespace, resource, err)
		}
	}
	if err := checkRegistryEndpoint(ctx, "container-registry.internal.home.geekcity.tech:32443"); err != nil {
		return PreflightResult{}, err
	}
	if err := checkRegistryEndpoint(ctx, "container-registry-mirror.internal.home.geekcity.tech:32443"); err != nil {
		return PreflightResult{}, err
	}
	return PreflightResult{KubeContext: contextName, AvailableTools: required}, nil
}

func (environment Environment) requireCan(ctx context.Context, verb, resource string) error {
	result, err := testkit.Command(ctx, "kubectl", "auth", "can-i", verb, resource, "-n", Namespace)
	if err != nil {
		return fmt.Errorf("check namespace permission %s %s: %w", verb, resource, err)
	}
	if strings.TrimSpace(string(result.Stdout)) != "yes" {
		return fmt.Errorf("missing namespace permission %s %s in %s", verb, resource, Namespace)
	}
	return nil
}

func checkRegistryEndpoint(ctx context.Context, endpoint string) error {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return fmt.Errorf("registry endpoint %s is not reachable: %w", endpoint, err)
	}
	return connection.Close()
}
