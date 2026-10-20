#!/usr/bin/env bash
set -euo pipefail

CLASH_ENV_SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$CLASH_ENV_SCRIPT_DIR/../../run.sh" environment inspect "$@"
