package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func LocalLock(id string) (func(), error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid run ID")
	}
	if err := os.MkdirAll(StateRoot(), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(StateRoot(), "."+id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("run %s is locked by another process", id)
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

type leaseObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		UID       string            `json:"uid,omitempty"`
		Version   string            `json:"resourceVersion,omitempty"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Holder   string `json:"holderIdentity"`
		Duration int    `json:"leaseDurationSeconds"`
		Renew    string `json:"renewTime"`
	} `json:"spec"`
}

const leaseTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func leaseTime() string { return time.Now().UTC().Format(leaseTimeLayout) }

func (e *Environment) Lease(ctx context.Context, id string, cancel context.CancelCauseFunc) (func(), error) {
	name := "clash-test-" + id
	var lease leaseObject
	output, err := e.Kubectl(ctx, nil, "get", "lease", name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, err
	}
	operation := "create"
	if len(output) > 0 {
		if err := json.Unmarshal(output, &lease); err != nil {
			return nil, err
		}
		if lease.Metadata.Labels[RunLabel] != id {
			return nil, fmt.Errorf("lease is not owned by this run")
		}
		renewedAt, err := time.Parse(leaseTimeLayout, lease.Spec.Renew)
		if err != nil {
			return nil, fmt.Errorf("lease has an invalid renewal timestamp: %w", err)
		}
		if time.Since(renewedAt) < time.Duration(lease.Spec.Duration)*time.Second {
			return nil, fmt.Errorf("run %s has an active namespace lease", id)
		}
		operation = "replace"
	}
	lease.APIVersion, lease.Kind = "coordination.k8s.io/v1", "Lease"
	lease.Metadata.Name, lease.Metadata.Namespace = name, e.Namespace
	lease.Metadata.Labels = map[string]string{RunLabel: id}
	lease.Spec.Holder = id + "-" + strconv.Itoa(os.Getpid()) + "-" + NewID()
	lease.Spec.Duration, lease.Spec.Renew = 90, leaseTime()
	data, _ := json.Marshal(lease)
	output, err = e.Kubectl(ctx, data, operation, "-f", "-", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("cannot acquire namespace lease: %w: %s", err, output)
	}
	if err := json.Unmarshal(output, &lease); err != nil {
		return nil, err
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				updated, err := e.renewLease(ctx, id, lease)
				if err != nil {
					cancel(fmt.Errorf("namespace lease renewal failed: %w", err))
					return
				}
				lease = updated
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		cleanup, release := context.WithTimeout(context.Background(), 15*time.Second)
		defer release()
		_ = e.deleteOwned(cleanup, Resource{Kind: "Lease", Name: name, UID: lease.Metadata.UID}, id)
	}, nil
}

func (e *Environment) renewLease(ctx context.Context, id string, lease leaseObject) (leaseObject, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		output, err := e.Kubectl(ctx, nil, "get", "lease", lease.Metadata.Name, "-o", "json")
		if err == nil {
			var current leaseObject
			if err = json.Unmarshal(output, &current); err == nil {
				if current.Metadata.UID != lease.Metadata.UID || current.Metadata.Labels[RunLabel] != id || current.Spec.Holder != lease.Spec.Holder {
					return lease, fmt.Errorf("lease ownership changed during renewal")
				}
				current.Spec.Duration = lease.Spec.Duration
				current.Spec.Renew = leaseTime()
				data, _ := json.Marshal(current)
				output, err = e.Kubectl(ctx, data, "replace", "-f", "-", "-o", "json")
				if err == nil {
					err = json.Unmarshal(output, &current)
					if err == nil {
						return current, nil
					}
				}
			}
		}
		last = fmt.Errorf("attempt %d: %w: %s", attempt+1, err, output)
		if attempt < 2 {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return lease, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lease, last
}
