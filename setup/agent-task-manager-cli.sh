#!/bin/bash

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
TARGET_DIR="${PROJECT_ROOT}/build/bin"
TARGET_BIN="${TARGET_DIR}/atmctl"
CLI_DIR="${PROJECT_ROOT}/application/agent-task-manager/cli/src"

function usage() {
    cat <<'EOF'
Usage: setup/agent-task-manager-cli.sh

Creates a local atmctl wrapper in build/bin/atmctl.
The wrapper runs the CLI from repository source via go run.

Environment:
  GO_BIN  Override the go executable path (default: go)

Output:
  Prints resolved atmctl wrapper path on stdout.
EOF
}

function ensure_go() {
    local go_bin="${GO_BIN:-go}"

    if ! command -v "$go_bin" >/dev/null 2>&1; then
        echo "Error: go is required to run atmctl from source" >&2
        return 1
    fi
}

function ensure_cli_dir() {
    if [[ ! -f "${CLI_DIR}/go.mod" ]]; then
        echo "Error: CLI module not found: ${CLI_DIR}" >&2
        return 1
    fi

    if [[ ! -d "${CLI_DIR}/cmd/atmctl" ]]; then
        echo "Error: CLI command directory not found: ${CLI_DIR}/cmd/atmctl" >&2
        return 1
    fi
}

function install_wrapper() {
    mkdir -p "$TARGET_DIR"

    cat > "$TARGET_BIN" <<EOF
#!/bin/bash

set -o errexit -o nounset -o pipefail

GO_BIN="\${GO_BIN:-go}"
CLI_DIR="${CLI_DIR}"

exec "\$GO_BIN" -C "\$CLI_DIR" run ./cmd/atmctl "\$@"
EOF

    chmod 0755 "$TARGET_BIN"
}

function main() {
    if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
        usage
        exit 0
    fi

    if [[ $# -ne 0 ]]; then
        usage >&2
        exit 1
    fi

    ensure_go
    ensure_cli_dir
    install_wrapper
    printf '%s\n' "$TARGET_BIN"
}

main "$@"
