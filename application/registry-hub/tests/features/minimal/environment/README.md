# feature-minimal Test Environment

Tests use the existing develop namespace in the current Kubernetes context. They do not create a namespace, StorageClass, PV, or cluster-scoped RBAC. Each run has a unique ID, Helm release, and registration ConfigMap. A local process lock and a namespace Lease prevent concurrent changes for the same ID. Tests do not inspect Pods in other namespaces and do not use port-forward.

## Prerequisites

- The active kubeconfig context can access develop and has the namespace-scoped permissions required by the suite.
- The cluster has a default StorageClass that dynamically provisions PVCs. The chart requests 20Gi for the backend and 100Gi for Distribution by default. Tests do not reduce these requests or create PVCs manually.
- Install Go 1.26, Helm, kubectl, and Podman. Podman must be able to log in to and push to container-registry.internal.home.geekcity.tech:32443. It must also allow a host-network container to access the workstation loopback address; the initializer-faults case uses this path to reach a local TLS API simulator. client-interop invokes the pinned ORAS CLI with go run oras.land/oras/cmd/oras@v1.2.3. The first run requires an available Go module proxy; the binary and dependencies are cached outside the repository.
- The workstation can reach the internal registry, the mirror, and the Service ClusterIP assigned to this release. Tests access the Registry and admin API directly through the Service IP and do not start port-forward.
- Cluster nodes can pull the backend, Distribution, NetworkPolicy probe, and VersityGW v1.8.0 images through the container-registry-mirror.internal.home.geekcity.tech:32443 prefix. VersityGW is pinned to manifest-list digest sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499. If the probe image is unavailable, set REGISTRY_HUB_TEST_PROBE_IMAGE to a reachable immutable image reference. Registry credentials must already be configured for Podman and the cluster.

## Check the Environment

From any working directory, run the script to check tools, the current context, namespace-scoped permissions, quota, LimitRange, and registry network access:

    application/registry-hub/tests/features/minimal/environment/scripts/check.sh

To reuse an already built digest image and skip the Podman command requirement:

    application/registry-hub/tests/features/minimal/environment/scripts/check.sh --no-build

The prerequisite check is read-only and does not need access to cluster-scoped StorageClass or PV resources. The installation case verifies that both PVCs are dynamically provisioned with the default StorageClass when no class is specified, reach Bound, and records the StorageClass assigned by Kubernetes. The check does not create resources, change proxy routes, or modify cluster permissions. The installation case also verifies direct access to the Service ClusterIP after deployment.

## Build and Deploy Images

A full run builds the backend from the current workspace using application/registry-hub/container/Containerfile and pushes it to:

    container-registry.internal.home.geekcity.tech:32443/registry-hub/backend:test-<id>

Deployment uses the resolved repository@sha256:... digest and does not depend on a mutable tag. The default build, runtime, and Distribution base images are pulled through the configured mirror. Set these environment variables to use reachable image references:

- REGISTRY_HUB_TEST_GO_IMAGE
- REGISTRY_HUB_TEST_RUNTIME_IMAGE
- REGISTRY_HUB_TEST_GOPROXY (default: https://goproxy.cn,direct)
- REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY
- REGISTRY_HUB_TEST_PLATFORM (default: linux/amd64)

An example Distribution image reference is container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/registry. The chart's current default version is used. Do not pre-create admin or internal Registry Secrets. The minimal [config.yaml](../config.yaml) is empty, and the initialization Job generates and persistently reuses the required credentials. Environment values override only the image repository and backend digest.

Use run --image <repository>@sha256:<digest> to specify an existing backend image. The report records prebuilt-image mode. The image must still be pullable by nodes in develop.

## Run and Preserve Failures

Run the full suite:

    application/registry-hub/tests/features/minimal/run.sh run

Print the deterministic plan without connecting to the cluster or building an image:

    application/registry-hub/tests/features/minimal/run.sh plan --case restart-reuse

If a case fails, environment creation fails, or the process is interrupted, the suite retains namespace resources and the report. Use the same ID to inspect, explicitly resume, or destroy the environment:

    application/registry-hub/tests/features/minimal/environment/scripts/inspect.sh --id <id>
    application/registry-hub/tests/features/minimal/run.sh run --id <id> --reuse --case <case-id>
    application/registry-hub/tests/features/minimal/environment/scripts/destroy.sh --id <id>

After all selected cases and dependencies pass, the runner uninstalls the release, verifies resource UIDs, and removes this run's PVCs, initialization-generated Secrets/ConfigMaps, and registration. Manual destruction is repeatable. The runner refuses deletion if an object's identity or UID does not match. PV reclamation follows the default StorageClass reclaimPolicy; the runner does not delete cluster-scoped PVs.

The external-combinations case creates immutable Secret fixtures under the current run ID to test switching between automatic and external modes before restoring empty configuration. Fixtures carry test-ID and instance labels. Successful cleanup and manual destruction remove them by recorded name and UID.

Reports and per-case output are stored outside the repository. The default path is $HOME/.local/state/registry-hub/feature-minimal/<id>/. If XDG_STATE_HOME is set, its corresponding directory is used. Reports contain Secret-content summaries only and do not store passwords, tokens, or private keys. The test binary is cached under $HOME/.cache/registry-hub/tests/ and is not written into the repository.

## Fixtures and Diagnostics

The default full plan includes 31 Go cases. external-combinations completes and restores empty values before the Registry fixtures are created. The DAG then runs protocol, paths, upload, artifact, Referrers, deletion, security, admin, persistence, failure, limits, observability, and real-client interoperability cases serially. client-interop uses temporary Podman VFS storage and ORAS v1.2.3 and Helm CLIs to test OCI artifact/chart round trips, referrer association, and deletion. It removes the temporary Podman storage afterward. Each fixture repository, digest, task ID, and resource name includes the current run ID; case outputs are passed only to declared consumers.

security-defaults probes TLS and authentication through the internal Registry ClusterIP using the generated CA and Basic credentials. A disposable probe Pod checks valid authentication, invalid authentication, an invalid CA, and an invalid service name. It carries the backend selector label but remains NotReady and is excluded from backend endpoints. Two other disposable Pods test NetworkPolicy: a Pod without the backend selector must be unable to connect, while a Pod with the selector is the positive control. Probe Pods mount only ca.crt; the TLS probe also mounts config.json. They do not mount a private key or use the product ServiceAccount and are removed after the case. Failures retain the release and are not silently skipped. The case also records permanent failure tasks caused by SSRF rejection.

dependency-failure creates a temporary diagnostic ClusterIP Service labeled with the run ID, sets publishNotReadyAddresses=true, and selects only the backend. It scales this run's Registry Deployment to zero, checks healthz, readyz, read/write/delete errors, then restores replicas and deletes the diagnostic Service. The Service exists only while the case runs. On failure, the release and diagnostic state remain available. To restore manually:

    kubectl -n develop scale deployment/<fullname>-registry --replicas=1
    kubectl -n develop rollout status deployment/<fullname>-registry --timeout=5m
    kubectl -n develop delete service <run-id-diagnostic-service> --ignore-not-found

Failure inspection never scans other namespaces and does not use port-forward. observability reads JSON logs from this run's backend Pod, the admin metrics endpoint, and Helm-rendered output. It checks operation-generated sequences, fixed low-cardinality labels, and Secret redaction. Missing probe conditions, namespace permissions, or direct Service access cause the case to fail and retain the environment; they are never silently skipped.

feature-backend-s3 creates a VersityGW v1.8.0 Pod, ClusterIP Service, immutable random-credential Secret, and separate bucket in develop, all labeled with the run ID. The image is pinned to manifest-list digest sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499. The POSIX backend writes to emptyDir. The fixture uses S3 SigV4, HTTP, and path-style addressing. Before deployment it checks bucket creation, conditional create/update, and ETag-based conditional deletion. The workstation and backend Pod access the Service ClusterIP/DNS directly; port-forward is not used. Objects live in emptyDir, so this scenario does not establish persistence across an S3 server restart. Passing runs clean up automatically; failures retain the fixture.

feature-registry-s3 uses the same pinned VersityGW image, with a run-specific service certificate and private CA. It serves a separate bucket over HTTPS using path-style addressing. The chart's registry.storage.s3.caSecret mounts the CA read-only into Distribution and sets SSL_CERT_FILE. The credentials Secret is exposed only to Distribution. The case checks rejection of invalid CA/credentials, OCI blob round trips larger than 10 MiB, object isolation by the Distribution rootDirectory, and object/content reads after restarting the Registry Deployment. Registry S3 does not request a Registry PVC; the backend continues to use its default FileSystem PVC. The workstation connects to the fixture directly by Service ClusterIP and certificate DNS name, without port-forward. VersityGW data is kept in emptyDir and does not establish persistence across object-store Pod restarts.
