#!/bin/bash

# Version utilities library
# Pure functions for version management operations
# No side effects, easy to test

set -o nounset -o pipefail

# Parse semver version into major, minor, patch components
# Usage: parse_semver "1.2.3"
# Output: "1 2 3"
function parse_semver() {
    local version="$1"

    # Validate semver format
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "Error: Invalid version format: $version (expected: X.Y.Z)" >&2
        return 1
    fi

    local major="${version%%.*}"
    local rest="${version#*.}"
    local minor="${rest%%.*}"
    local patch="${rest#*.}"

    echo "$major $minor $patch"
}

# Bump a semver version
# Usage: bump_semver "1.2.3" "patch|minor|major"
# Output: bumped version string
function bump_semver() {
    local version="$1"
    local bump_type="$2"

    local major minor patch
    read -r major minor patch <<< "$(parse_semver "$version")" || return 1

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
            echo "Error: Invalid bump type: $bump_type (must be major, minor, or patch)" >&2
            return 1
            ;;
    esac

    echo "${major}.${minor}.${patch}"
}

# Resolve VERSION file path for an application
# Usage: resolve_version_file_path "podman-in-container" "/path/to/application"
#        resolve_version_file_path "aria2/aria2" "/path/to/application"
# Output: absolute path to VERSION file
function resolve_version_file_path() {
    local app_path="$1"
    local base_dir="$2"
    local version_file=""

    if [[ "${app_path}" == *"/"* ]]; then
        # Multiple containers (e.g., aria2/aria2)
        local base_app="${app_path%%/*}"
        local container_name="${app_path#*/}"
        version_file="${base_dir}/${base_app}/container/${container_name}/VERSION"
    else
        # Single container (e.g., podman-in-container)
        version_file="${base_dir}/${app_path}/container/VERSION"
    fi

    echo "$version_file"
}

# Resolve Chart.yaml path for an application
# Usage: resolve_chart_file_path "podman-in-container" "/path/to/application"
# Output: absolute path to Chart.yaml
function resolve_chart_file_path() {
    local chart_name="$1"
    local base_dir="$2"

    # For chart, always use the base app name (no slash)
    local base_app_name="${chart_name%%/*}"
    echo "${base_dir}/${base_app_name}/chart/Chart.yaml"
}

# Resolve values.yaml path for an application
# Usage: resolve_values_file_path "podman-in-container" "/path/to/application"
# Output: absolute path to values.yaml
function resolve_values_file_path() {
    local chart_name="$1"
    local base_dir="$2"

    # For chart, always use the base app name (no slash)
    local base_app_name="${chart_name%%/*}"
    echo "${base_dir}/${base_app_name}/chart/values.yaml"
}

# Extract images metadata from Chart.yaml annotations
# Usage: extract_images_metadata "/path/to/Chart.yaml"
# Output: Multiple lines in format "name|path|valuesKey"
function extract_images_metadata() {
    local chart_file="$1"

    if [[ ! -f "$chart_file" ]]; then
        echo "Error: Chart file not found: $chart_file" >&2
        return 1
    fi

    # Use Python to parse YAML annotations
    python3 - "$chart_file" << 'PYTHON_SCRIPT'
import sys
import yaml

try:
    chart_file = sys.argv[1]
    with open(chart_file, 'r') as f:
        chart = yaml.safe_load(f)

    annotations = chart.get('annotations', {})
    images_yaml = annotations.get('k8s-at-home.io/images', '')

    if images_yaml:
        images = yaml.safe_load(images_yaml)
        if images:
            for img in images:
                print(f"{img['name']}|{img['path']}|{img['valuesKey']}")
except Exception as e:
    print(f"Error parsing Chart.yaml: {e}", file=sys.stderr)
    sys.exit(1)
PYTHON_SCRIPT
}

# Update a nested YAML value
# Usage: update_yaml_value "/path/to/file.yaml" "key.nested.path" "new_value"
function update_yaml_value() {
    local file="$1"
    local key_path="$2"
    local new_value="$3"

    if [[ ! -f "$file" ]]; then
        echo "Error: YAML file not found: $file" >&2
        return 1
    fi

    # Use Python to update nested YAML values
    python3 - "$file" "$key_path" "$new_value" << 'PYTHON_SCRIPT'
import sys
import yaml

try:
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
except Exception as e:
    print(f"Error updating YAML: {e}", file=sys.stderr)
    sys.exit(1)
PYTHON_SCRIPT
}

# Read value from YAML file
# Usage: read_yaml_value "/path/to/Chart.yaml" "version"
# Output: value as string
function read_yaml_value() {
    local file="$1"
    local key_path="$2"

    if [[ ! -f "$file" ]]; then
        echo "Error: YAML file not found: $file" >&2
        return 1
    fi

    python3 - "$file" "$key_path" << 'PYTHON_SCRIPT'
import sys
import yaml

try:
    file_path = sys.argv[1]
    key_path = sys.argv[2]

    with open(file_path, 'r') as f:
        data = yaml.safe_load(f)

    # Navigate to the nested key
    keys = key_path.split('.')
    current = data
    for key in keys:
        if isinstance(current, dict) and key in current:
            current = current[key]
        else:
            print(f"Error: Key '{key_path}' not found", file=sys.stderr)
            sys.exit(1)

    # Output the value (strip quotes if it's a string)
    value = str(current).strip('"').strip("'")
    print(value)
except Exception as e:
    print(f"Error reading YAML: {e}", file=sys.stderr)
    sys.exit(1)
PYTHON_SCRIPT
}

# Validate that a file exists
# Usage: validate_file_exists "/path/to/file" "VERSION"
function validate_file_exists() {
    local file_path="$1"
    local file_type="$2"

    if [[ ! -f "$file_path" ]]; then
        echo "Error: $file_type file not found at $file_path" >&2
        return 1
    fi
    return 0
}
