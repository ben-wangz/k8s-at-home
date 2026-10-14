package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/config"
	"strings"
)

func makeGCJob(cfg config.Config, name string) kubeJob {
	volumes := []map[string]any{{"name": "registry-config", "configMap": map[string]any{"name": cfg.GCConfigMap, "items": []map[string]string{{"key": cfg.GCConfigKey, "path": "config.yml"}}, "defaultMode": 420}}}
	mounts := []map[string]any{{"name": "registry-config", "mountPath": "/etc/distribution", "readOnly": true}, {"name": "tmp", "mountPath": "/tmp"}}
	if cfg.GCStoragePVC != "" {
		volumes = append(volumes, map[string]any{"name": "registry-storage", "persistentVolumeClaim": map[string]any{"claimName": cfg.GCStoragePVC}})
		mounts = append(mounts, map[string]any{"name": "registry-storage", "mountPath": "/var/lib/registry"})
	}
	serviceAccount := cfg.GCServiceAccount
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	podSpec := map[string]any{
		"restartPolicy": "Never", "serviceAccountName": serviceAccount, "automountServiceAccountToken": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000},
		"containers":      []map[string]any{{"name": "gc", "image": cfg.GCImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/bin/registry"}, "args": []string{"garbage-collect", "/etc/distribution/config.yml"}, "volumeMounts": mounts, "envFrom": gcJobEnvFrom(cfg.GCEnvSecret), "securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}}}},
		"volumes":         append(volumes, map[string]any{"name": "tmp", "emptyDir": map[string]any{}}),
	}
	if len(cfg.GCImagePullSecrets) > 0 {
		imagePullSecrets := make([]map[string]string, 0, len(cfg.GCImagePullSecrets))
		for _, secret := range cfg.GCImagePullSecrets {
			imagePullSecrets = append(imagePullSecrets, map[string]string{"name": secret.Name})
		}
		podSpec["imagePullSecrets"] = imagePullSecrets
	}
	return kubeJob{
		APIVersion: "batch/v1", Kind: "Job",
		Metadata: map[string]any{
			"name": name, "namespace": cfg.GCNamespace,
			"labels":      map[string]string{"app.kubernetes.io/name": "registry-hub", "app.kubernetes.io/component": "gc", "registry-hub.io/gc-id": strings.TrimPrefix(name, "registry-hub-gc-")},
			"annotations": map[string]string{gcConfigDigestAnnotation: gcJobConfigDigest(cfg)},
		},
		Spec: map[string]any{
			"parallelism": 1, "completions": 1, "backoffLimit": 0, "ttlSecondsAfterFinished": 604800,
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": "registry-hub", "app.kubernetes.io/component": "gc"}}, "spec": podSpec},
		},
	}
}

func gcJobEnvFrom(secret string) []map[string]any {
	if secret == "" {
		return nil
	}
	return []map[string]any{{"secretRef": map[string]string{"name": secret}}}
}

func (k *kubeAPI) createJob(ctx context.Context, job kubeJob) (jobStatus, error) {
	data, _ := json.Marshal(job)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, k.base+"/apis/batch/v1/namespaces/"+url.PathEscape(k.namespace)+"/jobs", bytes.NewReader(data))
	if err != nil {
		return jobStatus{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := k.do(request)
	if err != nil {
		return jobStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return jobStatus{}, errAlreadyExists
	}
	if response.StatusCode != http.StatusCreated {
		return jobStatus{}, fmt.Errorf("Kubernetes rejected GC Job creation with status %d", response.StatusCode)
	}
	var result jobStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return jobStatus{}, err
	}
	return result, nil
}

func (k *kubeAPI) getJob(ctx context.Context, name string) (jobStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/apis/batch/v1/namespaces/"+url.PathEscape(k.namespace)+"/jobs/"+url.PathEscape(name), nil)
	if err != nil {
		return jobStatus{}, err
	}
	response, err := k.do(request)
	if err != nil {
		return jobStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return jobStatus{}, errJobNotFound
	}
	if response.StatusCode != http.StatusOK {
		return jobStatus{}, fmt.Errorf("Kubernetes GC Job query returned status %d", response.StatusCode)
	}
	var result jobStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return jobStatus{}, err
	}
	return result, nil
}

func (k *kubeAPI) do(request *http.Request) (*http.Response, error) {
	request.Header.Set("Authorization", "Bearer "+k.token)
	response, err := k.client.Do(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func jobCondition(conditions []struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}, conditionType, status string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == status {
			return true
		}
	}
	return false
}
