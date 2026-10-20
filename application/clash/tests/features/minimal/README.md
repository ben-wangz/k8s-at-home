# Minimal scenario

`run.sh` is the independent Clash suite entry point and supports all registered scenarios. The minimal `config.yaml` keeps feature overrides empty. The environment supplies run-specific subscription, image, service, and resource values separately.

Use `run.sh list` to inspect cases and acceptance IDs, `run.sh plan` for dependency order, and `run.sh run --mock` for local browser execution. Run without `--mock` for real Mihomo integration. See the [suite guide](../../README.md) for prerequisites, evidence, and scenario selection.
