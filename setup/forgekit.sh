#!/bin/bash

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
TARGET_DIR="${PROJECT_ROOT}/build/bin"
TARGET_BIN="${TARGET_DIR}/forgekit"

DEFAULT_MIN_VERSION="${FORGEKIT_MIN_VERSION:-0.3.1}"
DEFAULT_BEST_VERSION="${FORGEKIT_BEST_VERSION:-0.3.1}"
FORGEKIT_REPO="${FORGEKIT_REPO:-ben-wangz/forgekit}"

function usage() {
    cat <<'EOF'
Usage: setup/forgekit.sh [min-version] [best-version]

Ensures a usable forgekit binary is available for this repository.

Arguments:
  min-version   Minimum acceptable forgekit version (default: FORGEKIT_MIN_VERSION or 0.3.1)
  best-version  Preferred forgekit version to download when install/upgrade is needed
                (default: FORGEKIT_BEST_VERSION or 0.3.1)

Environment:
  FORGEKIT_MIN_VERSION      Default min-version
  FORGEKIT_BEST_VERSION     Default best-version
  FORGEKIT_REPO             GitHub repo in owner/name form (default: ben-wangz/forgekit)
  FORGEKIT_DOWNLOAD_BASE    Override base URL, e.g. https://files.m.daocloud.io/github.com

Output:
  Prints resolved forgekit binary path on stdout.
EOF
}

function normalize_version() {
    local raw="$1"

    raw="${raw#v}"
    raw="${raw%%-*}"
    raw="${raw%%+*}"

    if [[ ! "$raw" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        return 1
    fi

    printf '%s\n' "$raw"
}

function version_ge() {
    local left
    local right

    left="$(normalize_version "$1")" || return 1
    right="$(normalize_version "$2")" || return 1

    local l_major l_minor l_patch
    local r_major r_minor r_patch
    IFS='.' read -r l_major l_minor l_patch <<< "$left"
    IFS='.' read -r r_major r_minor r_patch <<< "$right"

    if (( l_major > r_major )); then
        return 0
    fi
    if (( l_major < r_major )); then
        return 1
    fi

    if (( l_minor > r_minor )); then
        return 0
    fi
    if (( l_minor < r_minor )); then
        return 1
    fi

    if (( l_patch >= r_patch )); then
        return 0
    fi

    return 1
}

function read_forgekit_version() {
    local bin_path="$1"
    local output=""

    if ! output="$("${bin_path}" --version 2>/dev/null)"; then
        return 1
    fi

    if [[ "$output" =~ forgekit[[:space:]]+([^[:space:]]+) ]]; then
        normalize_version "${BASH_REMATCH[1]}"
        return $?
    fi

    return 1
}

function detect_os() {
    local os
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"

    case "$os" in
        linux|darwin)
            printf '%s\n' "$os"
            ;;
        *)
            echo "Error: Unsupported OS: ${os}" >&2
            return 1
            ;;
    esac
}

function detect_arch() {
    local arch
    arch="$(uname -m)"

    case "$arch" in
        x86_64|amd64)
            printf 'amd64\n'
            ;;
        aarch64|arm64)
            printf 'arm64\n'
            ;;
        *)
            echo "Error: Unsupported architecture: ${arch}" >&2
            return 1
            ;;
    esac
}

function download_with_curl() {
    local url="$1"
    local output_path="$2"
    curl -fsSL --retry 3 --connect-timeout 10 --max-time 120 -o "$output_path" "$url"
}

function install_release_binary() {
    local best_version="$1"
    local os
    local arch
    os="$(detect_os)"
    arch="$(detect_arch)"

    local version_tag="v${best_version}"
    local filename="forgekit_${os}_${arch}"
    local tmp_dir
    tmp_dir="$(mktemp -d)"
    trap 'rm -rf "${tmp_dir:-}"; trap - RETURN' RETURN

    local base_primary="https://github.com/${FORGEKIT_REPO}/releases/download/${version_tag}"
    local base_fallback="https://files.m.daocloud.io/github.com/${FORGEKIT_REPO}/releases/download/${version_tag}"

    if ! command -v curl >/dev/null 2>&1; then
        echo "Error: curl is required to download forgekit" >&2
        return 1
    fi

    local base_urls=()
    if [[ -n "${FORGEKIT_DOWNLOAD_BASE:-}" ]]; then
        base_urls=("${FORGEKIT_DOWNLOAD_BASE%/}/${FORGEKIT_REPO}/releases/download/${version_tag}")
    else
        base_urls=("${base_primary}" "${base_fallback}")
    fi

    local success=false
    for base in "${base_urls[@]}"; do
        if download_with_curl "${base}/${filename}" "${tmp_dir}/${filename}" && \
           download_with_curl "${base}/checksums.txt" "${tmp_dir}/checksums.txt"; then
            success=true
            break
        fi
    done

    if [[ "$success" != true ]]; then
        echo "Error: Failed to download forgekit ${version_tag}" >&2
        return 1
    fi

    if command -v sha256sum >/dev/null 2>&1; then
        if ! (cd "$tmp_dir" && sha256sum --check --status checksums.txt --ignore-missing); then
            echo "Error: Checksum verification failed for ${filename}" >&2
            return 1
        fi
    else
        echo "Warning: sha256sum not found, skipping checksum verification" >&2
    fi

    mkdir -p "$TARGET_DIR"
    install -m 0755 "${tmp_dir}/${filename}" "$TARGET_BIN"
}

function ensure_best_not_lower_than_min() {
    local min_version="$1"
    local best_version="$2"

    if ! version_ge "$best_version" "$min_version"; then
        echo "Error: best-version (${best_version}) is lower than min-version (${min_version})" >&2
        return 1
    fi
}

function main() {
    if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
        usage
        exit 0
    fi

    if [[ $# -gt 2 ]]; then
        usage >&2
        exit 1
    fi

    local min_input="${1:-${DEFAULT_MIN_VERSION}}"
    local best_input="${2:-${DEFAULT_BEST_VERSION}}"
    local min_version
    local best_version

    min_version="$(normalize_version "$min_input")" || {
        echo "Error: Invalid min-version: ${min_input}" >&2
        exit 1
    }
    best_version="$(normalize_version "$best_input")" || {
        echo "Error: Invalid best-version: ${best_input}" >&2
        exit 1
    }

    ensure_best_not_lower_than_min "$min_version" "$best_version"

    if [[ -x "$TARGET_BIN" ]]; then
        local target_version
        if target_version="$(read_forgekit_version "$TARGET_BIN")" && version_ge "$target_version" "$min_version"; then
            printf '%s\n' "$TARGET_BIN"
            exit 0
        fi
    fi

    if command -v forgekit >/dev/null 2>&1; then
        local system_bin
        local system_version
        system_bin="$(command -v forgekit)"
        if system_version="$(read_forgekit_version "$system_bin")" && version_ge "$system_version" "$min_version"; then
            mkdir -p "$TARGET_DIR"
            install -m 0755 "$system_bin" "$TARGET_BIN"
            printf '%s\n' "$TARGET_BIN"
            exit 0
        fi
    fi

    install_release_binary "$best_version"

    local installed_version
    installed_version="$(read_forgekit_version "$TARGET_BIN")" || {
        echo "Error: Failed to read installed forgekit version" >&2
        exit 1
    }
    if ! version_ge "$installed_version" "$min_version"; then
        echo "Error: Installed forgekit version (${installed_version}) is lower than min-version (${min_version})" >&2
        exit 1
    fi

    printf '%s\n' "$TARGET_BIN"
}

main "$@"
