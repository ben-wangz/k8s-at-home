package initialize

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	instanceLabel            = "registry-hub.io/instance"
	initializationLabel      = "registry-hub.io/managed-by"
	initializationValue      = "internal-initializer"
	adminInitializationValue = "admin-initializer"
	scopeAnnotation          = "registry-hub.io/internal-scope"
	adminScopeAnnotation     = "registry-hub.io/admin-scope"
	recordDataKey            = "record.json"
)

type record struct {
	Protocol  string `json:"protocol"`
	Instance  string `json:"instance"`
	Scope     string `json:"scope"`
	State     string `json:"state"`
	SecretUID string `json:"secretUID,omitempty"`
}

type managedTarget struct {
	namespace       string
	instance        string
	secretName      string
	recordName      string
	scope           string
	protocol        string
	managedBy       string
	scopeAnnotation string
	description     string
	generate        func() (map[string][]byte, error)
	validate        func(map[string][]byte) error
}

func Run(ctx context.Context, options Options) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if options.hasInternalConfiguration() && options.TLS {
		if err := validateServiceDNS(options.ServiceName, options.Namespace); err != nil {
			return err
		}
	}
	client, err := newKubeClient()
	if err != nil {
		return err
	}
	if options.hasInternalConfiguration() {
		target := internalTarget(options)
		if err := initializeTarget(ctx, client, target); err != nil {
			return err
		}
	}
	if options.AdminAuth {
		if err := initializeTarget(ctx, client, adminTarget(options)); err != nil {
			return err
		}
	}
	return nil
}

func internalTarget(options Options) managedTarget {
	return managedTarget{
		namespace: options.Namespace, instance: options.Instance, secretName: options.SecretName,
		recordName: options.RecordName, scope: options.Scope, protocol: protocolVersion,
		managedBy: initializationValue, scopeAnnotation: scopeAnnotation, description: "internal",
		generate: func() (map[string][]byte, error) { return generateSecretData(options) },
		validate: func(data map[string][]byte) error { return validateSecretData(data, options) },
	}
}

func adminTarget(options Options) managedTarget {
	return managedTarget{
		namespace: options.Namespace, instance: options.Instance, secretName: options.AdminSecretName,
		recordName: options.AdminRecordName, scope: options.AdminScope, protocol: adminProtocolVersion,
		managedBy: adminInitializationValue, scopeAnnotation: adminScopeAnnotation, description: "admin",
		generate: func() (map[string][]byte, error) { return generateAdminAuthData(options.AdminKey) },
		validate: func(data map[string][]byte) error { return validateAdminAuthData(data, options.AdminKey) },
	}
}

func initializeTarget(ctx context.Context, client *kubeClient, target managedTarget) error {
	state, err := ensurePendingRecord(ctx, client, target)
	if err != nil {
		return err
	}
	if state.State == "ready" {
		return verifyReadyState(ctx, client, target, state)
	}
	secret, err := ensureSecret(ctx, client, target)
	if err != nil {
		return err
	}
	return commitReadyRecord(ctx, client, target, secret)
}

func ensurePendingRecord(ctx context.Context, client *kubeClient, target managedTarget) (record, error) {
	for attempt := 0; attempt < 8; attempt++ {
		config, err := client.getConfigMap(ctx, target.namespace, target.recordName)
		if err == nil {
			state, err := parseRecord(config, target)
			return state, err
		}
		if !isNotFound(err) {
			return record{}, err
		}
		secret, secretErr := client.getSecret(ctx, target.namespace, target.secretName)
		if secretErr == nil {
			if err := validateSecret(secret, target); err != nil {
				return record{}, err
			}
			state := record{Protocol: target.protocol, Instance: target.instance, Scope: target.scope, State: "ready", SecretUID: secret.Metadata.UID}
			if err := client.createConfigMap(ctx, makeRecordObject(target, state)); err == nil {
				return state, nil
			} else if isAlreadyExists(err) {
				continue
			} else {
				observed, found, readErr := confirmRecordCreate(ctx, client, target, err)
				if readErr != nil {
					return record{}, readErr
				}
				if found {
					return observed, nil
				}
				return record{}, fmt.Errorf("create %s initialization record: %w", target.description, err)
			}
		}
		if !isNotFound(secretErr) {
			return record{}, secretErr
		}
		state := record{Protocol: target.protocol, Instance: target.instance, Scope: target.scope, State: "pending"}
		if err := client.createConfigMap(ctx, makeRecordObject(target, state)); err == nil {
			return state, nil
		} else if isAlreadyExists(err) {
			continue
		} else {
			observed, found, readErr := confirmRecordCreate(ctx, client, target, err)
			if readErr != nil {
				return record{}, readErr
			}
			if found {
				return observed, nil
			}
			return record{}, fmt.Errorf("create %s initialization record: %w", target.description, err)
		}
	}
	return record{}, fmt.Errorf("could not establish %s initialization state after concurrent updates", target.description)
}

func confirmRecordCreate(ctx context.Context, client *kubeClient, target managedTarget, createErr error) (record, bool, error) {
	object, err := client.getConfigMap(ctx, target.namespace, target.recordName)
	if isNotFound(err) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, fmt.Errorf("create %s initialization record: %w; confirm result: %v", target.description, createErr, err)
	}
	state, err := parseRecord(object, target)
	if err != nil {
		return record{}, false, err
	}
	return state, true, nil
}

func parseRecord(object *configMapObject, target managedTarget) (record, error) {
	if object.Metadata.Labels[instanceLabel] != target.instance || object.Metadata.Labels[initializationLabel] != target.managedBy || object.Metadata.Annotations[target.scopeAnnotation] != target.scope {
		return record{}, fmt.Errorf("%s initialization record belongs to a different instance or scope", target.description)
	}
	var state record
	if err := json.Unmarshal([]byte(object.Data[recordDataKey]), &state); err != nil {
		return record{}, fmt.Errorf("%s initialization record is invalid", target.description)
	}
	if state.Protocol != target.protocol || state.Instance != target.instance || state.Scope != target.scope || (state.State != "pending" && state.State != "ready") {
		return record{}, fmt.Errorf("%s initialization record is incompatible", target.description)
	}
	if state.State == "ready" && state.SecretUID == "" {
		return record{}, fmt.Errorf("ready %s initialization record has no Secret identity", target.description)
	}
	return state, nil
}

func ensureSecret(ctx context.Context, client *kubeClient, target managedTarget) (*secretObject, error) {
	secret, err := client.getSecret(ctx, target.namespace, target.secretName)
	if err == nil {
		if err := validateSecret(secret, target); err != nil {
			return nil, err
		}
		return secret, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	data, err := target.generate()
	if err != nil {
		return nil, err
	}
	if err := target.validate(data); err != nil {
		return nil, fmt.Errorf("generated %s configuration failed validation: %w", target.description, err)
	}
	candidate := makeSecretObject(target, data)
	createErr := client.createSecret(ctx, candidate)
	winner, readErr := client.getSecret(ctx, target.namespace, target.secretName)
	if readErr != nil {
		if createErr != nil {
			return nil, fmt.Errorf("create %s Secret: %w; confirm result: %v", target.description, createErr, readErr)
		}
		return nil, fmt.Errorf("read %s Secret after create: %w", target.description, readErr)
	}
	if err := validateSecret(winner, target); err != nil {
		return nil, err
	}
	return winner, nil
}

func validateSecret(secret *secretObject, target managedTarget) error {
	if secret.Metadata.Labels[instanceLabel] != target.instance || secret.Metadata.Labels[initializationLabel] != target.managedBy || secret.Metadata.Annotations[target.scopeAnnotation] != target.scope {
		return fmt.Errorf("%s Secret belongs to a different instance or scope", target.description)
	}
	if secret.Type != "Opaque" || !secret.Immutable {
		return fmt.Errorf("%s Secret is not an immutable Opaque Secret", target.description)
	}
	if secret.Metadata.UID == "" {
		return fmt.Errorf("%s Secret has no Kubernetes UID", target.description)
	}
	if err := target.validate(secret.Data); err != nil {
		return fmt.Errorf("%s Secret is invalid: %w", target.description, err)
	}
	return nil
}
