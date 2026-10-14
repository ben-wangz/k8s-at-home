package app

import (
	"encoding/json"
	"registry-hub/internal/config"
	"strings"
	"testing"
)

func TestValidateGCJobRejectsMutatedExecutionConfiguration(t *testing.T) {
	cfg := config.Config{
		GCImage:            "registry:3.1.2",
		GCImagePullSecrets: []config.ImagePullSecretReference{{Name: "registry-credentials"}},
		GCConfigMap:        "registry-config",
		GCConfigKey:        "config.yml",
		GCConfigDigest:     "config-sha256",
		GCStoragePVC:       "registry-data",
		GCEnvSecret:        "registry-storage-credentials",
		GCServiceAccount:   "registry-hub",
		GCNamespace:        "develop",
	}
	runID := strings.Repeat("a", 32)
	run := gcRun{ID: runID, ConfigDigest: gcJobConfigDigest(cfg)}
	name := "registry-hub-gc-" + runID[:12]

	newJob := func() jobStatus {
		encoded, err := json.Marshal(makeGCJob(cfg, name))
		if err != nil {
			t.Fatal(err)
		}
		var job jobStatus
		if err := json.Unmarshal(encoded, &job); err != nil {
			t.Fatal(err)
		}
		job.Metadata.UID = "job-uid"
		return job
	}

	valid := newJob()
	if err := validateGCJob(run, valid, cfg); err != nil {
		t.Fatalf("valid generated GC Job rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(jobStatus)
	}{
		{
			name: "parallelism",
			mutate: func(job jobStatus) {
				job.Spec["parallelism"] = 2
			},
		},
		{
			name: "completions",
			mutate: func(job jobStatus) {
				job.Spec["completions"] = 2
			},
		},
		{
			name: "backoff limit",
			mutate: func(job jobStatus) {
				job.Spec["backoffLimit"] = 1
			},
		},
		{
			name: "restart policy",
			mutate: func(job jobStatus) {
				pod := job.Spec["template"].(map[string]any)["spec"].(map[string]any)
				pod["restartPolicy"] = "OnFailure"
			},
		},
		{
			name: "image",
			mutate: func(job jobStatus) {
				pod := job.Spec["template"].(map[string]any)["spec"].(map[string]any)
				pod["containers"].([]any)[0].(map[string]any)["image"] = "attacker/registry:latest"
			},
		},
		{
			name: "command",
			mutate: func(job jobStatus) {
				pod := job.Spec["template"].(map[string]any)["spec"].(map[string]any)
				pod["containers"].([]any)[0].(map[string]any)["args"] = []any{"unexpected-command"}
			},
		},
		{
			name: "image pull secret",
			mutate: func(job jobStatus) {
				pod := job.Spec["template"].(map[string]any)["spec"].(map[string]any)
				pod["imagePullSecrets"].([]any)[0].(map[string]any)["name"] = "different-credentials"
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			job := newJob()
			test.mutate(job)
			if err := validateGCJob(run, job, cfg); err == nil {
				t.Fatal("mutated GC Job was accepted")
			}
		})
	}
}

func TestMakeGCJobUsesSingleAttemptAndConfiguredStorage(t *testing.T) {
	cfg := config.Config{
		GCImage:          "registry:3.1.2",
		GCConfigMap:      "registry-config",
		GCConfigKey:      "config.yml",
		GCStoragePVC:     "registry-data",
		GCServiceAccount: "registry-hub",
		GCNamespace:      "develop",
	}
	job := makeGCJob(cfg, "registry-hub-gc-0123456789ab")
	for field, want := range map[string]int{"parallelism": 1, "completions": 1, "backoffLimit": 0} {
		if got := job.Spec[field]; got != want {
			t.Errorf("Job %s = %v, want %d", field, got, want)
		}
	}
	podSpec := job.Spec["template"].(map[string]any)["spec"].(map[string]any)
	if got := podSpec["restartPolicy"]; got != "Never" {
		t.Errorf("restartPolicy = %v, want Never", got)
	}
	container := podSpec["containers"].([]map[string]any)[0]
	if got := container["image"]; got != cfg.GCImage {
		t.Errorf("GC image = %v, want %s", got, cfg.GCImage)
	}
	if !jsonValuesEqual(container["command"], []string{"/bin/registry"}) {
		t.Errorf("GC command = %v, want /bin/registry", container["command"])
	}
	if !jsonValuesEqual(container["args"], []string{"garbage-collect", "/etc/distribution/config.yml"}) {
		t.Errorf("GC args = %v, want configured Distribution garbage collection", container["args"])
	}
	wantVolumes := []map[string]any{
		{"name": "registry-config", "configMap": map[string]any{"name": "registry-config", "items": []map[string]string{{"key": "config.yml", "path": "config.yml"}}, "defaultMode": 420}},
		{"name": "registry-storage", "persistentVolumeClaim": map[string]any{"claimName": "registry-data"}},
		{"name": "tmp", "emptyDir": map[string]any{}},
	}
	if !jsonValuesEqual(podSpec["volumes"], wantVolumes) {
		t.Errorf("GC volumes = %v, want configured config and PVC volumes", podSpec["volumes"])
	}
}
