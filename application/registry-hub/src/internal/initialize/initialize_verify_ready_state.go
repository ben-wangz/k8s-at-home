package initialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

func verifyReadyState(ctx context.Context, client *kubeClient, target managedTarget, state record) error {
	secret, err := client.getSecret(ctx, target.namespace, target.secretName)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%s Secret is missing after initialization; refusing to regenerate credentials", target.description)
		}
		return err
	}
	if err := validateSecret(secret, target); err != nil {
		return err
	}
	if secret.Metadata.UID != state.SecretUID {
		return fmt.Errorf("%s Secret identity changed after initialization; refusing credential rotation", target.description)
	}
	return nil
}

func commitReadyRecord(ctx context.Context, client *kubeClient, target managedTarget, secret *secretObject) error {
	ready := record{Protocol: target.protocol, Instance: target.instance, Scope: target.scope, State: "ready", SecretUID: secret.Metadata.UID}
	for attempt := 0; attempt < 8; attempt++ {
		object, err := client.getConfigMap(ctx, target.namespace, target.recordName)
		if err != nil {
			return err
		}
		state, err := parseRecord(object, target)
		if err != nil {
			return err
		}
		if state.State == "ready" {
			if state.SecretUID != secret.Metadata.UID {
				return fmt.Errorf("%s initialization record references a different Secret", target.description)
			}
			return nil
		}
		encoded, _ := json.Marshal(ready)
		object.Data[recordDataKey] = string(encoded)
		if err := client.updateConfigMap(ctx, object); err == nil {
			return nil
		} else if isConflict(err) {
			continue
		} else {
			observed, readErr := client.getConfigMap(ctx, target.namespace, target.recordName)
			if readErr != nil {
				return fmt.Errorf("commit %s initialization record: %w; confirm result: %v", target.description, err, readErr)
			}
			current, parseErr := parseRecord(observed, target)
			if parseErr != nil {
				return parseErr
			}
			if current.State == "ready" && current.SecretUID == secret.Metadata.UID {
				return nil
			}
			if current.State == "ready" {
				return fmt.Errorf("%s initialization record references a different Secret", target.description)
			}
			return fmt.Errorf("commit %s initialization record: %w", target.description, err)
		}
	}
	return fmt.Errorf("could not commit %s initialization record after concurrent updates", target.description)
}

func makeSecretObject(target managedTarget, data map[string][]byte) *secretObject {
	return &secretObject{
		APIVersion: "v1", Kind: "Secret", Type: "Opaque", Immutable: true,
		Metadata: objectMeta{
			Name: target.secretName, Namespace: target.namespace,
			Labels:      map[string]string{instanceLabel: target.instance, initializationLabel: target.managedBy},
			Annotations: map[string]string{target.scopeAnnotation: target.scope},
		},
		Data: data,
	}
}

func makeRecordObject(target managedTarget, state record) *configMapObject {
	encoded, _ := json.Marshal(state)
	return &configMapObject{
		APIVersion: "v1", Kind: "ConfigMap",
		Metadata: objectMeta{
			Name: target.recordName, Namespace: target.namespace,
			Labels:      map[string]string{instanceLabel: target.instance, initializationLabel: target.managedBy},
			Annotations: map[string]string{target.scopeAnnotation: target.scope},
		},
		Data: map[string]string{recordDataKey: string(encoded)},
	}
}

func Wait(ctx context.Context, recordFile, scope, protocol string, timeout time.Duration) error {
	if recordFile == "" || scope == "" || protocol == "" || timeout <= 0 {
		return errors.New("initialization wait configuration is invalid")
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		data, err := readRecordFile(recordFile)
		if err == nil {
			var state record
			if decodeErr := json.Unmarshal(data, &state); decodeErr != nil {
				return errors.New("initialization record is invalid")
			}
			if state.Protocol != protocol || state.Scope != scope {
				return errors.New("initialization record does not match this workload")
			}
			if state.State == "ready" && state.SecretUID != "" {
				return nil
			}
			if state.State != "pending" {
				return errors.New("initialization record has an unsupported state")
			}
		} else if !errors.Is(err, errRecordUnavailable) {
			return err
		}
		select {
		case <-waitContext.Done():
			return errors.New("timed out waiting for initialization")
		case <-ticker.C:
		}
	}
}

var errRecordUnavailable = errors.New("initialization record is not projected yet")

func readRecordFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errRecordUnavailable
	}
	return data, err
}

func JobTimeout() time.Duration {
	return 5 * time.Minute
}
