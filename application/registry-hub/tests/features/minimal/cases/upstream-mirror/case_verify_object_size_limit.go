package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"time"
)

func verifyObjectSizeLimit(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	if err := setBackendEnvironment(ctx, environment,
		"REGISTRY_HUB_LIMIT_OBJECT_BYTES=1",
		"REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS=10000",
		"REGISTRY_HUB_LIMIT_CLOSURE_BYTES=1099511627776",
	); err != nil {
		return nil, fmt.Errorf("set controlled object-size limit: %w", err)
	}
	const reference = "minimal-object-size-limit"
	configPath := "/v2/registry-hub-test/fixture/blobs/" + support.Digest([]byte("object-size-limit-fixture"))
	configRequestsBefore, err := countFixturePath(ctx, environment, configPath)
	if err != nil {
		return nil, err
	}
	rootBody, rootHeaders, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("read object-size-limit root: %w", err)
	}
	rootDigest := support.Digest(rootBody)
	if rootHeaders.Get("Docker-Content-Digest") != rootDigest {
		return nil, fmt.Errorf("object-size-limit root digest header does not match its bytes")
	}
	task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, err
	}
	checkpoints := testkit.Map(task["checkpoints"])
	_, rootCheckpointExists := checkpoints[rootDigest]
	if testkit.String(task["errorClass"]) != "object_limit" || testkit.String(task["rootDigest"]) != rootDigest || task["rootPublished"] == true || task["retryable"] == true || rootCheckpointExists {
		return nil, fmt.Errorf("object-size-limit task has invalid terminal state: class=%q root=%q published=%v retryable=%v", testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], task["retryable"])
	}
	manifestRequests, err := countFixtureRequests(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	if manifestRequests != 1 {
		return nil, fmt.Errorf("object-size-limit manifest requests = %d, want one", manifestRequests)
	}
	configRequestsAfter, err := countFixturePath(ctx, environment, configPath)
	if err != nil {
		return nil, err
	}
	if configRequestsAfter != configRequestsBefore {
		return nil, fmt.Errorf("object-size-limit config blob requests changed from %d to %d", configRequestsBefore, configRequestsAfter)
	}
	return map[string]any{
		"objectSizeThreshold":            1,
		"objectSizeFixtureBytes":         len([]byte("object-size-limit-fixture")),
		"objectSizeTaskStatus":           testkit.String(task["status"]),
		"objectSizeErrorClass":           testkit.String(task["errorClass"]),
		"objectSizeRootUnpublished":      task["rootPublished"] != true,
		"objectSizeRootCheckpointAbsent": !rootCheckpointExists,
		"objectSizeManifestRequests":     manifestRequests,
		"objectSizeBlobRequestsBefore":   configRequestsBefore,
		"objectSizeRejectedBlobRequests": configRequestsAfter - configRequestsBefore,
	}, nil
}

func verifyClosureLimit(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository, reference, key string, threshold int, settings ...string) (map[string]any, error) {
	if err := setBackendEnvironment(ctx, environment, settings...); err != nil {
		return nil, fmt.Errorf("set controlled closure limit %s: %w", key, err)
	}
	rootBody, rootHeaders, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIIndexMediaType)
	if err != nil {
		return nil, fmt.Errorf("read %s root: %w", key, err)
	}
	rootDigest := support.Digest(rootBody)
	if rootHeaders.Get("Docker-Content-Digest") != rootDigest {
		return nil, fmt.Errorf("%s root digest header does not match its bytes", key)
	}
	task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, err
	}
	checkpoints := testkit.Map(task["checkpoints"])
	_, rootCheckpointExists := checkpoints[rootDigest]
	if testkit.String(task["errorClass"]) != "closure_limit" || testkit.String(task["rootDigest"]) != rootDigest || task["rootPublished"] == true || task["retryable"] == true || rootCheckpointExists {
		return nil, fmt.Errorf("%s task has invalid terminal state: class=%q root=%q published=%v retryable=%v", key, testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], task["retryable"])
	}
	requests, err := countFixtureRequests(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	if requests != 1 {
		return nil, fmt.Errorf("%s root fixture requests = %d, want one", key, requests)
	}
	return map[string]any{
		key + "Threshold":            threshold,
		key + "TaskStatus":           testkit.String(task["status"]),
		key + "ErrorClass":           testkit.String(task["errorClass"]),
		key + "RootDigest":           rootDigest,
		key + "RootUnpublished":      task["rootPublished"] != true,
		key + "RootCheckpointAbsent": !rootCheckpointExists,
		key + "FixtureRequests":      requests,
	}, nil
}

func setBackendEnvironment(ctx context.Context, environment testkit.CaseContext, entries ...string) error {
	arguments := []string{"-n", environment.Namespace, "set", "env", "deployment/" + environment.Fullname}
	arguments = append(arguments, entries...)
	if _, err := testkit.Command(ctx, "kubectl", arguments...); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
		return fmt.Errorf("backend did not restart: %w", err)
	}
	return upstreamauthtransport.WaitBackendServiceEndpoint(ctx, environment)
}

func restoreBackendRuntimeDefaults(ctx context.Context, environment testkit.CaseContext) error {
	return setBackendEnvironment(ctx, environment,
		"REGISTRY_HUB_PENDING_TASKS=1024",
		"REGISTRY_HUB_WORKERS=4",
		"REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=300",
		"REGISTRY_HUB_LIMIT_OBJECT_BYTES=107374182400",
		"REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS=10000",
		"REGISTRY_HUB_LIMIT_CLOSURE_BYTES=1099511627776",
	)
}

func restoreRequestTimeout(returnErr *error, environment testkit.CaseContext) {
	restoreCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := setBackendEnvironment(restoreCtx, environment, "REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=300"); err != nil {
		restoreErr := fmt.Errorf("restore default request timeout: %w", err)
		if *returnErr == nil {
			*returnErr = restoreErr
		} else {
			*returnErr = fmt.Errorf("%w; %v", *returnErr, restoreErr)
		}
	}
}
