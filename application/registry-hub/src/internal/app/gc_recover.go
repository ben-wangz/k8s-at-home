package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *gcController) Recover(ctx context.Context, confirmedNoExecutor bool) (gcRun, error) {
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return gcRun{}, errors.New("a GC executor is still active")
	}
	run := c.run
	c.mu.Unlock()
	if run.ID == "" || run.State == "succeeded" {
		return run, errors.New("there is no unresolved GC execution")
	}
	if run.Mode != "" && run.Mode != c.service.cfg.GCMode {
		return run, errors.New("GC mode changed since the unresolved run")
	}
	if c.service.cfg.GCMode == "kubernetes" && run.JobName != "" {
		api, err := c.kubernetesJobClient()
		if err != nil {
			return run, err
		}
		job, err := api.getJob(ctx, run.JobName)
		if err != nil {
			return run, err
		}
		if err := validateGCJob(run, job, c.service.cfg); err != nil {
			return run, err
		}
		if err := c.recordGCJobUID(ctx, run.ID, job.Metadata.UID); err != nil {
			return run, err
		}
		if job.Status.Succeeded == 0 && !jobCondition(job.Status.Conditions, "Complete", "True") {
			return run, errors.New("GC Job is not confirmed complete")
		}
	} else if !confirmedNoExecutor {
		return run, errors.New("recovery requires explicit confirmation that no GC process is running")
	}
	if run.Lifecycle && c.service.cfg.GCLifecycleExecutable != "" {
		lifecycleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.runLifecycle(lifecycleCtx, "exit-readonly")
		cancel()
		if err != nil {
			_ = c.service.gate.SetGCState(context.Background(), "failed_unknown", run.ExecutorID, "gc_lifecycle")
			return run, err
		}
	}
	if err := c.service.gate.SetGCState(ctx, "restore", run.ExecutorID, ""); err != nil {
		return run, err
	}
	if err := c.service.registry.Ping(ctx); err != nil {
		_ = c.service.gate.SetGCState(ctx, "failed_unknown", run.ExecutorID, "distribution_restore")
		return run, err
	}
	if err := c.completeWithState(ctx, "recovered"); err != nil {
		c.fail("storage", "failed_unknown")
		return run, err
	}
	return c.Snapshot(), nil
}

func (c *gcController) fail(class, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current := c.Snapshot()
	_ = c.service.gate.SetGCState(ctx, state, current.ExecutorID, class)
	c.mu.Lock()
	c.run.State = state
	c.run.ErrorClass = class
	c.run.UpdatedAt = time.Now().UTC()
	_ = c.persistLocked(ctx)
	c.mu.Unlock()
	c.service.logger.Warn("Distribution GC requires protected recovery", "gc_id", current.ID, "error_class", class)
}

func (c *gcController) update(state, executorID, jobName, jobUID string) {
	c.mu.Lock()
	if state != "" {
		c.run.State = state
	}
	if executorID != "" {
		c.run.ExecutorID = executorID
	}
	if jobName != "" {
		c.run.JobName = jobName
	}
	if jobUID != "" {
		c.run.JobUID = jobUID
	}
	c.run.UpdatedAt = time.Now().UTC()
	if err := c.persistLocked(context.Background()); err != nil {
		c.run.State = "failed_unknown"
		c.run.ErrorClass = "storage"
	}
	c.mu.Unlock()
}

func (c *gcController) persistLocked(ctx context.Context) error {
	data, err := json.Marshal(c.run)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: c.run.version == ""}
	if c.run.version != "" {
		options.IfMatch = c.run.version
	}
	entry, err := c.service.store.Put(ctx, "system/gc/last-run.json", bytes.NewReader(data), int64(len(data)), options)
	if err != nil {
		return err
	}
	c.run.version = entry.Version
	return nil
}

func randomRunID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type limitedBuffer struct {
	limit int
	data  []byte
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	if len(b.data) < b.limit {
		remaining := b.limit - len(b.data)
		if len(data) > remaining {
			data = data[:remaining]
		}
		b.data = append(b.data, data...)
	}
	return originalLength, nil
}

var errAlreadyExists = errors.New("Kubernetes object already exists")
var errJobNotFound = errors.New("Kubernetes GC Job does not exist")
var errGCJobStatusUnknown = errors.New("Kubernetes GC Job state is unknown")

type kubeAPI struct {
	namespace string
	base      string
	token     string
	client    *http.Client
}

func newKubeAPI(namespace string) (*kubeAPI, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" || namespace == "" {
		return nil, errors.New("in-cluster Kubernetes API configuration is unavailable")
	}
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		return nil, errors.New("Kubernetes service account token is unavailable")
	}
	caData, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, errors.New("Kubernetes service account CA is unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, errors.New("Kubernetes service account CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, Proxy: nil, ResponseHeaderTimeout: 15 * time.Second}
	return &kubeAPI{namespace: namespace, base: "https://" + net.JoinHostPort(host, port), token: strings.TrimSpace(string(token)), client: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type kubeJob struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

type jobStatus struct {
	Metadata struct {
		UID         string            `json:"uid"`
		Annotations map[string]string `json:"annotations"`
		Labels      map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status struct {
		Succeeded  int `json:"succeeded"`
		Failed     int `json:"failed"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

const gcConfigDigestAnnotation = "registry-hub.io/config-digest"

func gcJobConfigDigest(cfg config.Config) string {
	serviceAccount := cfg.GCServiceAccount
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	identity := struct {
		Image, ConfigMap, ConfigKey, ConfigDigest, StoragePVC, EnvSecret, ServiceAccount, Namespace string
		ImagePullSecrets                                                                            []config.ImagePullSecretReference
	}{cfg.GCImage, cfg.GCConfigMap, cfg.GCConfigKey, cfg.GCConfigDigest, cfg.GCStoragePVC, cfg.GCEnvSecret, serviceAccount, cfg.GCNamespace, cfg.GCImagePullSecrets}
	data, _ := json.Marshal(identity)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
