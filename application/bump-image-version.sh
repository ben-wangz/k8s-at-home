#!/bin/bash

set -o errexit -o nounset -o pipefail

# Script to bump container image versions
# Usage:
#   bash bump-image-version.sh <app-path> <major|minor|patch>
#
# Examples:
#   bash bump-image-version.sh podman-in-container patch
#   bash bump-image-version.sh aria2/aria2 minor
#   bash bump-image-version.sh aria2/aria-ng major

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
Usage: $0 <app-path> <bump-type>

Bump container image version following semver.

Arguments:
  app-path    Image path (e.g., podman-in-container, aria2/aria2, aria2/aria-ng)
  bump-type   Version bump type: major, minor, or patch

Examples:
  $0 podman-in-container patch    # 1.2.0 -> 1.2.1
  $0 aria2/aria2 minor            # 1.0.0 -> 1.1.0
  $0 aria2/aria-ng major          # 1.0.0 -> 2.0.0

EOF
    exit 1
}

function get_version_file() {
    local app_path="$1"
    local version_file=""

    if [[ "${app_path}" == *"/"* ]]; then
        # Multiple containers (e.g., aria2/aria2)
        local base_app="${app_path%%/*}"
        local container_name="${app_path#*/}"
        version_file="${SCRIPT_DIR}/${base_app}/container/${container_name}/VERSION"
    else
        # Single container (e.g., podman-in-container)
        version_file="${SCRIPT_DIR}/${app_path}/container/VERSION"
    fi

    if [[ ! -f "${version_file}" ]]; then
        log_error "VERSION file not found at ${version_file}"
        exit 1
    fi

    echo "${version_file}"
}

function parse_version() {
    local version="$1"

    # Validate semver format
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        log_error "Invalid version format: $version (expected: X.Y.Z)"
        exit 1
    fi

    local major="${version%%.*}"
    local rest="${version#*.}"
    local minor="${rest%%.*}"
    local patch="${rest#*.}"

    echo "$major $minor $patch"
}

function bump_version() {
    local version="$1"
    local bump_type="$2"

    read -r major minor patch <<< "$(parse_version "$version")"

    case "$bump_type" in
        major)
            major=$((major + 1))
            minor=0
            patch=0
            ;;
        minor)
            minor=$((minor + 1))
            patch=0
            ;;
        patch)
            patch=$((patch + 1))
            ;;
        *)
            log_error "Invalid bump type: $bump_type (must be major, minor, or patch)"
            exit 1
            ;;
    esac

    echo "${major}.${minor}.${patch}"
}

# Main
if [[ $# -ne 2 ]]; then
    usage
fi

APP_PATH="$1"
BUMP_TYPE="$2"

log_info "Bumping image version for: $APP_PATH"

# Get version file path
VERSION_FILE=$(get_version_file "$APP_PATH")
log_info "VERSION file: $VERSION_FILE"

# Read current version
CURRENT_VERSION=$(cat "$VERSION_FILE")
log_info "Current version: $CURRENT_VERSION"

# Calculate new version
NEW_VERSION=$(bump_version "$CURRENT_VERSION" "$BUMP_TYPE")
log_info "New version: $NEW_VERSION"

# Confirm before making changes
read -p "Do you want to update the version? (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    log_warn "Aborted by user"
    exit 0
fi

# Update VERSION file
echo "$NEW_VERSION" > "$VERSION_FILE"
log_info "✓ Updated $VERSION_FILE"

echo ""
log_info "Version bump complete!"
log_info "  $APP_PATH: $CURRENT_VERSION -> $NEW_VERSION"
echo ""
log_warn "Next steps:"
log_warn "  1. Review the changes"
log_warn "  2. Update the chart version if needed: bash bump-chart-version.sh <chart-name> <bump-type> --sync-images"
log_warn "  3. Commit the changes"
