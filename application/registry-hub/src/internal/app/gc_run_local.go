package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"registry-hub/internal/config"
	"syscall"
	"time"
)

func (c *gcController) runLocal(ctx context.Context) error {
	lifecycleEnabled := c.service.cfg.GCLifecycleExecutable != ""
	if lifecycleEnabled {
		if err := c.runLifecycle(ctx, "enter-readonly"); err != nil {
			return err
		}
	}
	command := exec.CommandContext(ctx, c.service.cfg.GCExecutable, "garbage-collect", c.service.cfg.GCConfig)
	command.Dir = filepath.Dir(c.service.cfg.GCConfig)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &limitedBuffer{limit: 16 << 10}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		c.service.logger.Warn("Distribution GC executable failed", "error_class", "gc_executor")
		return err
	}
	if lifecycleEnabled {
		// Keep the Distribution protected after an unsuccessful GC. The
		// recovery endpoint performs the same action after operator review.
		if err := c.runLifecycle(ctx, "exit-readonly"); err != nil {
			return err
		}
	}
	return nil
}

func (c *gcController) runLifecycle(ctx context.Context, action string) error {
	if c.service.cfg.GCLifecycleExecutable == "" {
		return nil
	}
	args := []string{action}
	if c.service.cfg.GCLifecycleConfig != "" {
		args = append(args, c.service.cfg.GCLifecycleConfig)
	}
	command := exec.CommandContext(ctx, c.service.cfg.GCLifecycleExecutable, args...)
	command.Dir = filepath.Dir(c.service.cfg.GCLifecycleExecutable)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &limitedBuffer{limit: 16 << 10}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		c.service.logger.Warn("Distribution lifecycle adapter failed", "action", action, "error_class", "gc_lifecycle")
		return err
	}
	return nil
}

func (c *gcController) runJob(ctx context.Context, executorID string) error {
	api, err := c.kubernetesJobClient()
	if err != nil {
		return err
	}
	name := "registry-hub-gc-" + c.Snapshot().ID[:12]
	c.update("gc_running", executorID, name, "")
	job := makeGCJob(c.service.cfg, name)
	created, err := api.createJob(ctx, job)
	if err != nil {
		if !errors.Is(err, errAlreadyExists) {
			return fmt.Errorf("%w: create GC Job: %v", errGCJobStatusUnknown, err)
		}
		created, err = api.getJob(ctx, name)
		if err != nil {
			return fmt.Errorf("%w: read existing GC Job: %v", errGCJobStatusUnknown, err)
		}
	}
	if err := validateGCJob(c.Snapshot(), created, c.service.cfg); err != nil {
		return fmt.Errorf("%w: validate created GC Job: %v", errGCJobStatusUnknown, err)
	}
	c.update("gc_running", executorID, name, created.Metadata.UID)
	for {
		current, err := api.getJob(ctx, name)
		if err != nil {
			return fmt.Errorf("%w: query Kubernetes GC Job: %v", errGCJobStatusUnknown, err)
		}
		if err := validateGCJob(c.Snapshot(), current, c.service.cfg); err != nil || current.Metadata.UID != created.Metadata.UID {
			return fmt.Errorf("%w: GC Job identity or configuration changed while running", errGCJobStatusUnknown)
		}
		if current.Status.Succeeded > 0 || jobCondition(current.Status.Conditions, "Complete", "True") {
			return nil
		}
		if current.Status.Failed > 0 || jobCondition(current.Status.Conditions, "Failed", "True") {
			return errors.New("Kubernetes GC Job failed")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *gcController) Retry(ctx context.Context, confirmedNoExecutor bool) (gcRun, error) {
	if !confirmedNoExecutor {
		return gcRun{}, errors.New("retry requires explicit confirmation that no GC executor is running")
	}
	previous := c.Snapshot()
	if previous.Mode != "" && previous.Mode != c.service.cfg.GCMode {
		return gcRun{}, errors.New("GC mode changed since the unresolved run")
	}
	if c.service.cfg.GCMode == "kubernetes" && previous.JobName != "" {
		api, err := c.kubernetesJobClient()
		if err != nil {
			return gcRun{}, err
		}
		job, err := api.getJob(ctx, previous.JobName)
		if err == nil {
			if err := validateGCJob(previous, job, c.service.cfg); err != nil {
				return gcRun{}, err
			}
			if err := c.recordGCJobUID(ctx, previous.ID, job.Metadata.UID); err != nil {
				return gcRun{}, err
			}
			if job.Status.Succeeded > 0 || jobCondition(job.Status.Conditions, "Complete", "True") {
				return gcRun{}, errors.New("previous GC Job completed; use recover instead of retry")
			}
			if !jobCondition(job.Status.Conditions, "Failed", "True") {
				return gcRun{}, errors.New("previous GC Job is active or has an unknown state")
			}
		} else if errors.Is(err, errJobNotFound) {
			return gcRun{}, errors.New("previous Kubernetes GC Job is missing; recovery is required")
		} else {
			return gcRun{}, err
		}
	}
	return c.Trigger(true)
}

func validateGCJob(run gcRun, job jobStatus, cfg config.Config) error {
	if len(run.ID) < 12 || job.Metadata.UID == "" || job.Metadata.Labels["registry-hub.io/gc-id"] != run.ID[:12] || run.JobUID != "" && job.Metadata.UID != run.JobUID {
		return errors.New("GC Job identity changed; operation is refused")
	}
	configDigest := gcJobConfigDigest(cfg)
	if run.ConfigDigest == "" || run.ConfigDigest != configDigest || job.Metadata.Annotations[gcConfigDigestAnnotation] != run.ConfigDigest {
		return errors.New("GC Job configuration identity changed; operation is refused")
	}
	if err := validateGCJobSpec(job.Spec, cfg); err != nil {
		return err
	}
	return nil
}

func (c *gcController) kubernetesJobClient() (gcJobClient, error) {
	if c.jobClient != nil {
		return c.jobClient, nil
	}
	return newKubeAPI(c.service.cfg.GCNamespace)
}

func validateGCJobSpec(actual map[string]any, cfg config.Config) error {
	expected := makeGCJob(cfg, "registry-hub-gc-000000000000").Spec
	for _, field := range []string{"parallelism", "completions", "backoffLimit", "ttlSecondsAfterFinished"} {
		if !jsonValuesEqual(actual[field], expected[field]) {
			return errors.New("GC Job execution policy changed; operation is refused")
		}
	}
	actualTemplate, actualOK := stringAnyMap(actual["template"])
	expectedTemplate, expectedOK := stringAnyMap(expected["template"])
	if !actualOK || !expectedOK {
		return errors.New("GC Job pod template is missing; operation is refused")
	}
	actualPod, actualOK := stringAnyMap(actualTemplate["spec"])
	expectedPod, expectedOK := stringAnyMap(expectedTemplate["spec"])
	if !actualOK || !expectedOK {
		return errors.New("GC Job pod configuration is missing; operation is refused")
	}
	for _, field := range []string{"restartPolicy", "serviceAccountName", "automountServiceAccountToken", "imagePullSecrets"} {
		if !jsonValuesEqual(actualPod[field], expectedPod[field]) {
			return errors.New("GC Job pod identity changed; operation is refused")
		}
	}
	if gcPodSecurityContextChanged(actualPod["securityContext"], expectedPod["securityContext"]) || gcPodHasHostAccess(actualPod) {
		return errors.New("GC Job pod security changed; operation is refused")
	}
	if !validateGCContainers(actualPod, expectedPod) || !validateGCVolumes(actualPod["volumes"], expectedPod["volumes"]) {
		return errors.New("GC Job container or storage configuration changed; operation is refused")
	}
	return nil
}

func gcPodSecurityContextChanged(actual, expected any) bool {
	_, actualOK := stringAnyMap(actual)
	_, expectedOK := stringAnyMap(expected)
	if !actualOK || !expectedOK {
		return true
	}
	return !jsonValuesEqual(actual, expected)
}

func gcPodHasHostAccess(pod map[string]any) bool {
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC", "shareProcessNamespace", "hostUsers"} {
		if value, ok := pod[field].(bool); ok && value {
			return true
		}
	}
	return false
}
