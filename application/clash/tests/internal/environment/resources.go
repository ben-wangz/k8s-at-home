package environment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type object struct {
	Type       string         `json:"type,omitempty"`
	Spec       map[string]any `json:"spec,omitempty"`
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace,omitempty"`
		UID       string            `json:"uid,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Data map[string]string `json:"data,omitempty"`
}

func (e *Environment) apply(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = e.Kubectl(ctx, data, "create", "-f", "-")
	return err
}

func (e *Environment) labeled(record *Record, kind, name string) object {
	value := object{APIVersion: "v1", Kind: kind}
	value.Metadata.Name, value.Metadata.Namespace = name, e.Namespace
	value.Metadata.Labels = map[string]string{RunLabel: record.ID}
	return value
}

func (e *Environment) Collect(ctx context.Context, record *Record) error {
	var resources []Resource
	for _, selection := range []struct{ kinds, label string }{
		{"deployments,services,configmaps,secrets,pods,ingresses,persistentvolumeclaims", RunLabel + "=" + record.ID},
		{"secrets", "owner=helm,name=" + record.Release},
	} {
		output, err := e.Kubectl(ctx, nil, "get", selection.kinds, "-l", selection.label, "-o", "json")
		if err != nil {
			return err
		}
		var list struct {
			Items []object `json:"items"`
		}
		if err := json.Unmarshal(output, &list); err != nil {
			return err
		}
		for _, item := range list.Items {
			if item.Kind == "Pod" {
				continue
			}
			var content any = item.Data
			if item.Kind == "Deployment" {
				content = item.Spec["template"]
			}
			fingerprint, _ := json.Marshal(content)
			resources = append(resources, Resource{Kind: item.Kind, Name: item.Metadata.Name, UID: item.Metadata.UID, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(fingerprint))})
		}
	}
	if len(record.Resources) > 0 {
		previous := make(map[string]string)
		fingerprints := make(map[string]string)
		for _, resource := range record.Resources {
			previous[resource.Kind+"/"+resource.Name] = resource.UID
			fingerprints[resource.Kind+"/"+resource.Name] = resource.Fingerprint
		}
		for _, resource := range resources {
			if uid := previous[resource.Kind+"/"+resource.Name]; uid != "" && uid != resource.UID {
				return fmt.Errorf("resource identity changed: %s/%s", resource.Kind, resource.Name)
			}
			if old := fingerprints[resource.Kind+"/"+resource.Name]; old != "" && old != resource.Fingerprint {
				return fmt.Errorf("resource contents changed: %s/%s", resource.Kind, resource.Name)
			}
		}
	}
	record.Resources = resources
	return nil
}

func ResourcePath(namespace string, resource Resource) (string, error) {
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	if !valid.MatchString(namespace) || !valid.MatchString(resource.Name) || strings.Contains(namespace, ".") || strings.Contains(resource.Name, "..") {
		return "", fmt.Errorf("invalid cleanup resource name")
	}
	prefix, plural := "/api/v1/", ""
	switch resource.Kind {
	case "Deployment":
		prefix, plural = "/apis/apps/v1/", "deployments"
	case "Ingress":
		prefix, plural = "/apis/networking.k8s.io/v1/", "ingresses"
	case "Lease":
		prefix, plural = "/apis/coordination.k8s.io/v1/", "leases"
	case "Service":
		plural = "services"
	case "Secret":
		plural = "secrets"
	case "ConfigMap":
		plural = "configmaps"
	case "PersistentVolumeClaim":
		plural = "persistentvolumeclaims"
	default:
		return "", fmt.Errorf("unsupported cleanup resource %s", resource.Kind)
	}
	return prefix + "namespaces/" + namespace + "/" + plural + "/" + resource.Name, nil
}

func (e *Environment) deleteOwned(ctx context.Context, resource Resource, id string) error {
	path, err := ResourcePath(e.Namespace, resource)
	if err != nil {
		return err
	}
	if resource.UID == "" {
		return fmt.Errorf("resource UID required for deletion")
	}
	output, err := e.Kubectl(ctx, nil, "get", resource.Kind, resource.Name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return err
	}
	if len(output) == 0 {
		return nil
	}
	var current object
	if err := json.Unmarshal(output, &current); err != nil {
		return err
	}
	if current.Metadata.UID != resource.UID {
		return fmt.Errorf("refusing deletion: UID changed for %s/%s", resource.Kind, resource.Name)
	}
	owned := current.Metadata.Labels[RunLabel] == id
	helmOwned := resource.Kind == "Secret" && current.Metadata.Labels["owner"] == "helm" && current.Metadata.Labels["name"] == "clash-test-"+id
	if !owned && !helmOwned {
		return fmt.Errorf("resource ownership labels do not match run %s", id)
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": resource.UID}})
	if _, err = e.Kubectl(ctx, body, "delete", "--raw", path, "-f", "-"); err != nil {
		return err
	}
	_, err = e.Kubectl(ctx, nil, "wait", "--for=delete", resource.Kind+"/"+resource.Name, "--timeout=120s")
	return err
}

func (e *Environment) Destroy(ctx context.Context, directory string, record *Record) error {
	if record.Context != e.Context || record.Namespace != e.Namespace || record.Release != "clash-test-"+record.ID {
		return fmt.Errorf("cleanup environment identity mismatch")
	}
	if err := e.Collect(ctx, record); err != nil {
		return err
	}
	record.State = "cleaning"
	if err := Save(directory, record); err != nil {
		return err
	}
	resources := append([]Resource(nil), record.Resources...)
	sort.SliceStable(resources, func(i, j int) bool { return resources[i].Kind == "Deployment" && resources[j].Kind != "Deployment" })
	for _, resource := range resources {
		if err := e.deleteOwned(ctx, resource, record.ID); err != nil {
			return err
		}
	}
	record.State, record.CleanupError = "destroyed", ""
	return Save(directory, record)
}

func (e *Environment) Diagnostics(directory string, record *Record) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for name, arguments := range map[string][]string{
		"resources.txt": {"get", "deployments,services,pods,ingresses", "-l", RunLabel + "=" + record.ID, "-o", "wide"},
		"pods.log":      {"logs", "-l", RunLabel + "=" + record.ID, "--all-containers", "--tail=100", "--prefix=true"},
	} {
		output, _ := e.Kubectl(ctx, nil, arguments...)
		_ = os.WriteFile(filepath.Join(directory, name), output, 0600)
	}
	_ = e.Collect(ctx, record)
	_ = Save(directory, record)
}
