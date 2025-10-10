#!/bin/bash

set -o errexit -o nounset -o pipefail

# Script to bump Helm chart versions
# Usage:
#   bash bump-chart-version.sh <chart-name> <major|minor|patch> [--sync-images]
#
# Examples:
#   bash bump-chart-version.sh aria2 patch
#   bash bump-chart-version.sh podman-in-container minor --sync-images

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
APPLICATION_DIR="${PROJECT_ROOT}/application"

# Load version utilities library
source "${SCRIPT_DIR}/lib/version-utils.sh"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

function log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

function log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

function log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

function usage() {
    cat << EOF
Usage: $0 <chart-name> <bump-type> [--sync-images]

Bump Helm chart version following semver.

Arguments:
  chart-name  Chart name (e.g., aria2, podman-in-container)
  bump-type   Version bump type: major, minor, or patch

Options:
  --sync-images   Sync image versions from VERSION files to Chart.yaml appVersion
                  and values.yaml image tags

Examples:
  $0 aria2 patch                          # Bump chart version only
  $0 podman-in-container minor            # Bump chart version only
  $0 aria2 patch --sync-images            # Bump chart version and sync image versions

EOF
    exit 1
}

# Main
if [[ $# -lt 2 ]] || [[ $# -gt 3 ]]; then
    usage
fi

CHART_NAME="$1"
BUMP_TYPE="$2"
SYNC_IMAGES=false

if [[ $# -eq 3 ]]; then
    if [[ "$3" == "--sync-images" ]]; then
        SYNC_IMAGES=true
    else
        log_error "Unknown option: $3"
        usage
    fi
fi

CHART_FILE=$(resolve_chart_file_path "$CHART_NAME" "$APPLICATION_DIR")
VALUES_FILE=$(resolve_values_file_path "$CHART_NAME" "$APPLICATION_DIR")

if ! validate_file_exists "$CHART_FILE" "Chart.yaml"; then
    exit 1
fi

log_info "Bumping chart version for: $CHART_NAME"
log_info "Chart file: $CHART_FILE"

# Read current chart version
CURRENT_VERSION=$(read_yaml_value "$CHART_FILE" "version")
log_info "Current chart version: $CURRENT_VERSION"

# Calculate new version
if ! NEW_VERSION=$(bump_semver "$CURRENT_VERSION" "$BUMP_TYPE"); then
    log_error "Failed to calculate new version"
    exit 1
fi
log_info "New chart version: $NEW_VERSION"

# Handle image synchronization
IMAGES_TO_SYNC=()
if [[ "$SYNC_IMAGES" == true ]]; then
    log_info ""
    log_info "Syncing image versions..."

    # Get images metadata from Chart.yaml annotations
    while IFS='|' read -r name path valuesKey; do
        if [[ -n "$name" ]]; then
            # Get version file path
            img_version_file=$(resolve_version_file_path "$path" "$APPLICATION_DIR")

            # Get image version from VERSION file
            if [[ -f "$img_version_file" ]]; then
                img_version=$(cat "$img_version_file")
                IMAGES_TO_SYNC+=("$name|$path|$valuesKey|$img_version")
                log_info "  - $name ($path): $img_version"
            else
                log_warn "  - $name ($path): VERSION file not found, skipping"
            fi
        fi
    done < <(extract_images_metadata "$CHART_FILE")

    if [[ ${#IMAGES_TO_SYNC[@]} -eq 0 ]]; then
        log_warn "No images to sync (no k8s-at-home.io/images annotation found)"
        SYNC_IMAGES=false
    fi
fi

# Show summary
echo ""
log_info "=== Summary ==="
log_info "Chart version: $CURRENT_VERSION -> $NEW_VERSION"
if [[ "$SYNC_IMAGES" == true ]]; then
    log_info "Image synchronization: enabled"
    for entry in "${IMAGES_TO_SYNC[@]}"; do
        IFS='|' read -r name path valuesKey img_version <<< "$entry"
        log_info "  - $valuesKey: $img_version"
    done
else
    log_info "Image synchronization: disabled"
fi
echo ""

# Confirm before making changes
read -p "Do you want to proceed with these changes? (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    log_warn "Aborted by user"
    exit 0
fi

# Update Chart.yaml version
sed -i "s/^version: ${CURRENT_VERSION}$/version: ${NEW_VERSION}/" "$CHART_FILE"
log_info "✓ Updated chart version in $CHART_FILE"

# Sync images if requested
if [[ "$SYNC_IMAGES" == true ]]; then
    # Determine appVersion (use first image version or existing appVersion)
    FIRST_IMG_VERSION=""
    if [[ ${#IMAGES_TO_SYNC[@]} -gt 0 ]]; then
        IFS='|' read -r name path valuesKey img_version <<< "${IMAGES_TO_SYNC[0]}"
        FIRST_IMG_VERSION="$img_version"
    fi

    if [[ -n "$FIRST_IMG_VERSION" ]]; then
        # Update appVersion in Chart.yaml
        CURRENT_APP_VERSION=$(read_yaml_value "$CHART_FILE" "appVersion")
        sed -i "s/^appVersion: \"${CURRENT_APP_VERSION}\"$/appVersion: \"${FIRST_IMG_VERSION}\"/" "$CHART_FILE"
        log_info "✓ Updated appVersion to $FIRST_IMG_VERSION in $CHART_FILE"
    fi

    # Update image tags in values.yaml
    for entry in "${IMAGES_TO_SYNC[@]}"; do
        IFS='|' read -r name path valuesKey img_version <<< "$entry"
        update_yaml_value "$VALUES_FILE" "$valuesKey" "$img_version"
        log_info "✓ Updated $valuesKey to $img_version in values.yaml"
    done
fi

echo ""
log_info "Chart version bump complete!"
log_info "  Chart: $CHART_NAME"
log_info "  Version: $CURRENT_VERSION -> $NEW_VERSION"
if [[ "$SYNC_IMAGES" == true ]]; then
    log_info "  Images: synced"
fi
echo ""
log_warn "Next steps:"
log_warn "  1. Review the changes: git diff"
log_warn "  2. Test the chart: bash tests/ct-lint.sh $CHART_NAME"
log_warn "  3. Commit the changes"
