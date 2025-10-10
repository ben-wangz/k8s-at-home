#!/bin/bash

set -o errexit -o nounset -o pipefail

# Script to get version information for applications
# Usage:
#   bash get-version.sh                      # List all versions
#   bash get-version.sh <app-name> [image|chart]
#
# Examples:
#   bash get-version.sh                     # List all chart and image versions
#   bash get-version.sh podman-in-container image
#   bash get-version.sh podman-in-container chart
#   bash get-version.sh aria2/aria2 image
#   bash get-version.sh aria2/aria-ng image
#   bash get-version.sh aria2 chart

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

function usage() {
    echo "Usage: $0 [app-name] [image|chart]"
    echo ""
    echo "Get version information for an application or list all versions"
    echo ""
    echo "Arguments:"
    echo "  (no args)   List all chart and image versions"
    echo "  app-name    Application name (e.g., podman-in-container, aria2, aria2/aria2)"
    echo "  type        Version type: 'image' or 'chart' (default: chart)"
    echo ""
    echo "Examples:"
    echo "  $0                              # List all versions"
    echo "  $0 podman-in-container image    # Get container image version"
    echo "  $0 podman-in-container chart    # Get helm chart version"
    echo "  $0 podman-in-container          # Get helm chart version (default)"
    echo "  $0 aria2/aria2 image            # Get aria2 container image version"
    echo "  $0 aria2/aria-ng image          # Get aria-ng container image version"
    echo "  $0 aria2 chart                  # Get aria2 helm chart version"
    exit 1
}

function list_all_versions() {
    echo "=== Helm Charts ==="
    echo ""

    # Find all Chart.yaml files
    while IFS= read -r chart_file; do
        local app_dir=$(dirname "$(dirname "$chart_file")")
        local app_name=$(basename "$app_dir")
        local version=$(grep '^version:' "$chart_file" | awk '{print $2}')
        local app_version=$(grep '^appVersion:' "$chart_file" | awk '{print $2}' | tr -d '"')

        printf "  %-25s chart: %-10s appVersion: %s\n" "$app_name" "$version" "$app_version"
    done < <(find "${SCRIPT_DIR}" -maxdepth 3 -type f -name "Chart.yaml" | sort)

    echo ""
    echo "=== Container Images ==="
    echo ""

    # Find all VERSION files
    while IFS= read -r version_file; do
        local version=$(cat "$version_file")
        local container_dir=$(dirname "$version_file")
        local relative_path=${container_dir#${SCRIPT_DIR}/}

        # Parse the path to get app name
        if [[ "$relative_path" == */container ]]; then
            # Single container (e.g., podman-in-container/container)
            local app_name=$(basename "$(dirname "$container_dir")")
            printf "  %-25s image: %s\n" "$app_name" "$version"
        else
            # Multiple containers (e.g., aria2/container/aria2 or aria2/container/aria-ng)
            local container_name=$(basename "$container_dir")
            local base_app=$(basename "$(dirname "$(dirname "$container_dir")")")
            printf "  %-25s image: %s\n" "$base_app/$container_name" "$version"
        fi
    done < <(find "${SCRIPT_DIR}" -type f -name "VERSION" | sort)

    echo ""
}

function get_image_version() {
    local app_path="$1"
    local version_file=""

    # Check if app_path contains a slash (e.g., aria2/aria2 or aria2/aria-ng)
    if [[ "${app_path}" == *"/"* ]]; then
        # Split the path: base_app/container_name -> base_app/container/container_name/VERSION
        local base_app="${app_path%%/*}"
        local container_name="${app_path#*/}"
        version_file="${SCRIPT_DIR}/${base_app}/container/${container_name}/VERSION"
    else
        # Single-level path (e.g., podman-in-container)
        version_file="${SCRIPT_DIR}/${app_path}/container/VERSION"
    fi

    if [[ ! -f "${version_file}" ]]; then
        echo "Error: VERSION file not found at ${version_file}" >&2
        exit 1
    fi

    cat "${version_file}"
}

function get_chart_version() {
    local app_name="$1"
    # For chart, use the base app name (e.g., aria2 instead of aria2/aria2)
    local base_app_name="${app_name%%/*}"
    local chart_file="${SCRIPT_DIR}/${base_app_name}/chart/Chart.yaml"

    if [[ ! -f "${chart_file}" ]]; then
        echo "Error: Chart.yaml not found at ${chart_file}" >&2
        exit 1
    fi

    grep '^version:' "${chart_file}" | awk '{print $2}'
}

function get_app_version() {
    local app_name="$1"
    # For appVersion, use the base app name
    local base_app_name="${app_name%%/*}"
    local chart_file="${SCRIPT_DIR}/${base_app_name}/chart/Chart.yaml"

    if [[ ! -f "${chart_file}" ]]; then
        echo "Error: Chart.yaml not found at ${chart_file}" >&2
        exit 1
    fi

    grep '^appVersion:' "${chart_file}" | awk '{print $2}' | tr -d '"'
}

# Main
if [[ $# -eq 0 ]]; then
    # No arguments: list all versions
    list_all_versions
    exit 0
elif [[ $# -gt 2 ]]; then
    usage
fi

APP_NAME="$1"
VERSION_TYPE="${2:-chart}"

case "${VERSION_TYPE}" in
    image)
        get_image_version "${APP_NAME}"
        ;;
    chart)
        get_chart_version "${APP_NAME}"
        ;;
    app|appVersion)
        get_app_version "${APP_NAME}"
        ;;
    *)
        echo "Error: Invalid version type '${VERSION_TYPE}'. Must be 'image' or 'chart'" >&2
        usage
        ;;
esac
