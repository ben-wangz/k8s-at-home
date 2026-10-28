# Registry Hub

Registry Hub provides a local-first OCI Distribution endpoint backed by a separately deployed CNCF Distribution instance. Pulls check the internal Registry first. Public upstreams are queried only when the logical repository contains an authority and the internal Registry reports a miss. Push and Delete requests write only to the internal Registry.

See the [configuration guide](docs/configuration.md) for the minimal Helm values, default storage requests, generated credentials, and initialization Job. The chart provisions separate PVCs for the backend and Distribution. The initialization Job generates and persistently reuses admin credentials, internal authentication, TLS certificates, and the upload session secret. Client authentication is disabled by default for trusted networks.

The backend admin API reference is generated from Go API definitions and served at the authenticated /docs page. The OpenAPI 3.1 document is available at /openapi.yaml.

The [test guide](tests/features/minimal/README.md) describes automated regression runs for the minimal configuration and the develop test environment.

## Documentation

The docs directory contains configuration and operations guides. Configuration examples link directly to the config.yaml for each test feature.

| Document | Coverage |
| --- | --- |
| [Configuration](docs/configuration.md) | Minimal installation and individual feature settings |
| [Usage](docs/usage.md) | Images, mirrors, Helm OCI, ORAS, and Referrers |
| [Security](docs/security.md) | Admin credentials, permissions, upstream settings, and Secret lifecycle |
| [Operations](docs/operations.md) | Tasks, cleanup, GC, upgrades, backup, restore, and uninstall |
| [Image inventory](docs/inventory.md) | Reference/platform inventory, cache coverage, lifecycle, and immutable pagination |
| [Observability](docs/observability.md) | Health checks, logs, metrics, and alerts |
| [Architecture](docs/architecture.md) | Component boundaries, storage responsibilities, and core flows |
| [Standalone](docs/standalone.md) | Process mode, authentication/TLS, persistent storage, and local GC |

## Build the Backend

Run this command from the repository root:

    go build -C application/registry-hub/src -o registry-hub ./cmd/registry-hub

See [Security](docs/security.md) and [Standalone](docs/standalone.md) for environment variables. Standalone GC uses absolute paths in REGISTRY_HUB_GC_EXECUTABLE and REGISTRY_HUB_GC_CONFIG. The default storage backend is FileSystem; only a single backend instance is supported.
