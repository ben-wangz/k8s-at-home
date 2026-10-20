# Test environment

Environment commands operate on a unique Clash run in an existing namespace. They use a local lock, namespace Lease, run-specific labels, and recorded resource UIDs. The runner creates no namespace, StorageClass, PV, or cluster-scoped RBAC.

- `scripts/check.sh` performs prerequisite checks without modifying the environment.
- `scripts/create.sh --id <id>` provisions fixtures and the Clash release.
- `scripts/inspect.sh --id <id>` prints the saved run record and diagnostic locations.
- `scripts/destroy.sh --id <id>` deletes only the resources owned by the recorded run.

All scripts work by absolute path from any directory. Failed runs remain available for inspection and explicit reuse; successful test runs clean up resources and retain reports. See the [suite guide](../../../README.md) for configuration and recovery commands.
