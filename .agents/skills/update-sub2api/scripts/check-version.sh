#!/usr/bin/env bash

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
CHART_DIR="${PROJECT_ROOT}/application/sub2api/chart"

function require_command() {
    local command_name="$1"
    if ! command -v "$command_name" >/dev/null 2>&1; then
        echo "Error: required command not found: ${command_name}" >&2
        exit 1
    fi
}

function resolve_gh() {
    if [[ -n "${GH_BIN:-}" ]]; then
        command -v "$GH_BIN"
        return
    fi

    if [[ -x "${PROJECT_ROOT}/build/bin/gh" ]]; then
        printf '%s\n' "${PROJECT_ROOT}/build/bin/gh"
        return
    fi

    command -v gh
}

function is_semver() {
    [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

function version_gt() {
    local left="$1"
    local right="$2"
    [[ "$left" != "$right" ]] && \
        [[ "$(printf '%s\n%s\n' "$left" "$right" | sort -V | tail -n 1)" == "$left" ]]
}

require_command jq
require_command sort
require_command tail
require_command yq

GH_BIN="$(resolve_gh)" || {
    echo "Error: required command not found: gh" >&2
    exit 1
}

export NO_COLOR=1
export GH_NO_UPDATE_NOTIFIER=1
export GH_PAGER=cat
export GH_PROMPT_DISABLED=1

release_data="$("$GH_BIN" api repos/Wei-Shaw/sub2api/releases/latest \
    --jq '[.tag_name, .html_url, .published_at] | @tsv')"
IFS=$'\t' read -r upstream_tag upstream_url published_at <<< "$release_data"
upstream_version="${upstream_tag#v}"

image_version="$(yq -r '.image.tag // ""' "${CHART_DIR}/values.yaml")"

for version in "$upstream_version" "$image_version"; do
    if ! is_semver "$version"; then
        echo "Error: invalid semantic version: ${version:-<empty>}" >&2
        exit 1
    fi
done

update_required=false
comparison=current

if version_gt "$upstream_version" "$image_version"; then
    update_required=true
    comparison=upstream-newer
elif version_gt "$image_version" "$upstream_version"; then
    comparison=local-newer
fi

jq -n \
    --arg upstreamVersion "$upstream_version" \
    --arg upstreamTag "$upstream_tag" \
    --arg upstreamUrl "$upstream_url" \
    --arg publishedAt "$published_at" \
    --arg imageVersion "$image_version" \
    --arg comparison "$comparison" \
    --argjson updateRequired "$update_required" \
    '{
        upstreamVersion: $upstreamVersion,
        upstreamTag: $upstreamTag,
        upstreamUrl: $upstreamUrl,
        publishedAt: $publishedAt,
        imageVersion: $imageVersion,
        updateRequired: $updateRequired,
        comparison: $comparison
    }'
