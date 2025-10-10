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

function get_images_metadata() {
    local chart_file="$1"

    # Extract images metadata using Python
    python3 - "$chart_file" << 'PYTHON_SCRIPT'
import sys
import yaml

chart_file = sys.argv[1]

with open(chart_file, 'r') as f:
    chart = yaml.safe_load(f)

annotations = chart.get('annotations', {})
images_yaml = annotations.get('k8s-at-home.io/images', '')

if images_yaml:
    images = yaml.safe_load(images_yaml)
    for img in images:
        print(f"{img['name']}|{img['path']}|{img['valuesKey']}")
PYTHON_SCRIPT
}

function update_yaml_value() {
    local file="$1"
    local key="$2"
    local value="$3"

    # Use Python to update nested YAML values
    python3 - "$file" "$key" "$value" << 'PYTHON_SCRIPT'
import sys
import yaml

file_path = sys.argv[1]
key_path = sys.argv[2]
new_value = sys.argv[3]

with open(file_path, 'r') as f:
    data = yaml.safe_load(f)

# Navigate to the nested key
keys = key_path.split('.')
current = data
for key in keys[:-1]:
    if key not in current:
        current[key] = {}
    current = current[key]

# Set the value
current[keys[-1]] = new_value

with open(file_path, 'w') as f:
    yaml.dump(data, f, default_flow_style=False, allow_unicode=True, sort_keys=False)
PYTHON_SCRIPT
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

CHART_FILE="${SCRIPT_DIR}/${CHART_NAME}/chart/Chart.yaml"
VALUES_FILE="${SCRIPT_DIR}/${CHART_NAME}/chart/values.yaml"

if [[ ! -f "$CHART_FILE" ]]; then
    log_error "Chart.yaml not found at $CHART_FILE"
    exit 1
fi

log_info "Bumping chart version for: $CHART_NAME"
log_info "Chart file: $CHART_FILE"

# Read current chart version
CURRENT_VERSION=$(grep '^version:' "$CHART_FILE" | awk '{print $2}')
log_info "Current chart version: $CURRENT_VERSION"

# Calculate new version
NEW_VERSION=$(bump_version "$CURRENT_VERSION" "$BUMP_TYPE")
log_info "New chart version: $NEW_VERSION"

# Handle image synchronization
IMAGES_TO_SYNC=()
if [[ "$SYNC_IMAGES" == true ]]; then
    log_info ""
    log_info "Syncing image versions..."

    # Get images metadata from Chart.yaml annotations
    while IFS='|' read -r name path valuesKey; do
        if [[ -n "$name" ]]; then
            # Get image version from VERSION file
            img_version=$(bash "${SCRIPT_DIR}/get-version.sh" "$path" image 2>/dev/null || echo "")
            if [[ -n "$img_version" ]]; then
                IMAGES_TO_SYNC+=("$name|$path|$valuesKey|$img_version")
                log_info "  - $name ($path): $img_version"
            else
                log_warn "  - $name ($path): VERSION file not found, skipping"
            fi
        fi
    done < <(get_images_metadata "$CHART_FILE")

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
        CURRENT_APP_VERSION=$(grep '^appVersion:' "$CHART_FILE" | awk '{print $2}' | tr -d '"')
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
