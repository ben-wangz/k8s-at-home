package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type ServiceEndpoint struct {
	Address string
	Port    int64
}

func (environment Environment) Deploy(ctx context.Context, release, baseValues, valuesFile string) error {
	_, err := testkit.Command(ctx, "helm", "upgrade", "--install", release, environment.ChartDir,
		"--namespace", Namespace,
		"--values", baseValues,
		"--values", valuesFile,
		"--wait", "--wait-for-jobs", "--timeout", "15m")
	return err
}

func (environment Environment) Uninstall(ctx context.Context, release string) error {
	_, err := testkit.Command(ctx, "helm", "uninstall", release, "--namespace", Namespace, "--ignore-not-found", "--wait", "--timeout", "5m")
	return err
}

func (environment Environment) Service(ctx context.Context, name, portName string) (ServiceEndpoint, error) {
	object, err := testkit.KubectlGet(ctx, Namespace, "service", name)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	spec := testkit.Map(object["spec"])
	ip := testkit.String(spec["clusterIP"])
	if ip == "" || ip == "None" {
		return ServiceEndpoint{}, fmt.Errorf("service %s has no ClusterIP", name)
	}
	for _, raw := range testkit.Slice(spec["ports"]) {
		port := testkit.Map(raw)
		if testkit.String(port["name"]) == portName {
			number := testkit.Int(port["port"])
			if number < 1 || number > 65535 {
				return ServiceEndpoint{}, fmt.Errorf("service %s has an invalid port", name)
			}
			return ServiceEndpoint{Address: net.JoinHostPort(ip, fmt.Sprint(number)), Port: number}, nil
		}
	}
	return ServiceEndpoint{}, fmt.Errorf("service %s has no port %q", name, portName)
}

func (environment Environment) Restart(ctx context.Context, fullname string) error {
	for _, name := range []string{fullname, fullname + "-registry"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "rollout", "restart", "deployment/"+name); err != nil {
			return err
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "rollout", "status", "deployment/"+name, "--timeout=5m"); err != nil {
			return err
		}
	}
	return nil
}

func (environment Environment) Ready(ctx context.Context, fullname string) error {
	for _, name := range []string{fullname, fullname + "-registry"} {
		object, err := testkit.KubectlGet(ctx, Namespace, "deployment", name)
		if err != nil {
			return err
		}
		status := testkit.Map(object["status"])
		spec := testkit.Map(object["spec"])
		expected := testkit.Int(spec["replicas"])
		if expected == 0 {
			expected = 1
		}
		if testkit.Int(status["readyReplicas"]) != expected || testkit.Int(status["availableReplicas"]) != expected {
			return fmt.Errorf("deployment %s is not fully available", name)
		}
	}
	return nil
}

func (environment Environment) ReleaseExists(ctx context.Context, release string) (bool, error) {
	result, err := testkit.Command(ctx, "helm", "list", "--namespace", Namespace, "--filter", "^"+release+"$", "--output", "json")
	if err != nil {
		return false, err
	}
	var releases []map[string]any
	if err := json.Unmarshal(result.Stdout, &releases); err != nil {
		return false, fmt.Errorf("decode Helm releases: %w", err)
	}
	return len(releases) > 0, nil
}

func boundedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}
