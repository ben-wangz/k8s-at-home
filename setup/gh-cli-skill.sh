#!/bin/bash

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
TARGET_DIR="${PROJECT_ROOT}/.opencode/skills/gh-cli"

DEFAULT_REPO="${GH_CLI_SKILL_REPO:-rrebollo/opencode-gh-cli}"
DEFAULT_REF="${GH_CLI_SKILL_REF:-master}"
DEFAULT_SUBDIR="${GH_CLI_SKILL_SUBDIR:-skills/gh-cli}"

function usage() {
    cat <<'EOF'
Usage: setup/gh-cli-skill.sh [repo] [ref] [subdir]

Installs or upgrades local .opencode gh-cli skill files from a GitHub repository.

Arguments:
  repo    GitHub repo in owner/name form (default: GH_CLI_SKILL_REPO or rrebollo/opencode-gh-cli)
  ref     Git ref (branch/tag/commit) to download (default: GH_CLI_SKILL_REF or master)
  subdir  Skill directory inside the repository (default: GH_CLI_SKILL_SUBDIR or skills/gh-cli)

Environment:
  GH_CLI_SKILL_REPO    Default source repository
  GH_CLI_SKILL_REF     Default source ref
  GH_CLI_SKILL_SUBDIR  Default source subdirectory

Output:
  Prints updated local skill directory path on stdout.
EOF
}

function validate_repo() {
    local repo="$1"
    if [[ ! "$repo" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
        echo "Error: Invalid repo format: ${repo} (expected owner/name)" >&2
        return 1
    fi
}

function require_binary() {
    local name="$1"
    if ! command -v "$name" >/dev/null 2>&1; then
        echo "Error: required command not found: ${name}" >&2
        return 1
    fi
}

function upgrade_skill() {
    local repo="$1"
    local ref="$2"
    local subdir="$3"

    local archive_url="https://codeload.github.com/${repo}/tar.gz/${ref}"

    local tmp_dir
    tmp_dir="$(mktemp -d)"
    trap 'rm -rf "${tmp_dir:-}"; trap - RETURN' RETURN

    local archive_file="${tmp_dir}/skill.tar.gz"
    curl -fsSL --retry 3 --connect-timeout 10 --max-time 180 -o "$archive_file" "$archive_url"

    tar -xzf "$archive_file" -C "$tmp_dir"

    local source_dir=""
    source_dir="$(find "$tmp_dir" -mindepth 2 -maxdepth 4 -type d -path "*/${subdir}" | head -n 1)"
    if [[ -z "$source_dir" ]]; then
        echo "Error: Skill directory not found in archive: ${subdir}" >&2
        return 1
    fi

    local mode="install"
    if [[ -d "$TARGET_DIR" ]]; then
        mode="upgrade"
    fi

    rm -rf "$TARGET_DIR"
    mkdir -p "$(dirname "$TARGET_DIR")"
    cp -R "$source_dir" "$TARGET_DIR"

    echo "gh-cli skill ${mode} complete: ${repo}@${ref} (${subdir})" >&2

    printf '%s\n' "$TARGET_DIR"
}

function main() {
    if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
        usage
        exit 0
    fi

    if [[ $# -gt 3 ]]; then
        usage >&2
        exit 1
    fi

    local repo="${1:-${DEFAULT_REPO}}"
    local ref="${2:-${DEFAULT_REF}}"
    local subdir="${3:-${DEFAULT_SUBDIR}}"

    validate_repo "$repo"
    require_binary curl
    require_binary tar
    require_binary find

    upgrade_skill "$repo" "$ref" "$subdir"
}

main "$@"
