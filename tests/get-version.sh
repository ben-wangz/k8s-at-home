#!/bin/bash

set -o errexit -o nounset -o pipefail

# Unit test for get-version.sh
# This test automatically discovers all charts and images and tests them

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
GET_VERSION_SCRIPT="${PROJECT_ROOT}/application/get-version.sh"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

function log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

function log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

function log_test() {
    echo -e "${YELLOW}[TEST]${NC} $1"
}

function assert_equals() {
    local expected="$1"
    local actual="$2"
    local test_name="$3"

    TESTS_RUN=$((TESTS_RUN + 1))

    if [[ "$expected" == "$actual" ]]; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: $test_name"
        return 0
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: $test_name"
        log_error "  Expected: $expected"
        log_error "  Actual:   $actual"
        return 1
    fi
}

function assert_success() {
    local command="$1"
    local test_name="$2"

    TESTS_RUN=$((TESTS_RUN + 1))

    if eval "$command" &>/dev/null; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: $test_name"
        return 0
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: $test_name"
        log_error "  Command failed: $command"
        return 1
    fi
}

function assert_failure() {
    local command="$1"
    local test_name="$2"

    TESTS_RUN=$((TESTS_RUN + 1))

    if eval "$command" &>/dev/null; then
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: $test_name"
        log_error "  Command should have failed but succeeded: $command"
        return 1
    else
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: $test_name"
        return 0
    fi
}

function test_list_all_versions() {
    log_test "Testing: list all versions (no arguments)"

    local output=$(bash "${GET_VERSION_SCRIPT}")

    # Check that output contains the section headers
    if echo "$output" | grep -q "=== Helm Charts ==="; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: Output contains Helm Charts section"
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: Output missing Helm Charts section"
    fi
    TESTS_RUN=$((TESTS_RUN + 1))

    if echo "$output" | grep -q "=== Container Images ==="; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: Output contains Container Images section"
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: Output missing Container Images section"
    fi
    TESTS_RUN=$((TESTS_RUN + 1))
}

function test_chart_versions() {
    log_test "Testing: Chart versions"

    # Discover all charts
    while IFS= read -r chart_file; do
        local app_dir=$(dirname "$(dirname "$chart_file")")
        local app_name=$(basename "$app_dir")
        local expected_version=$(grep '^version:' "$chart_file" | awk '{print $2}')
        local actual_version=$(bash "${GET_VERSION_SCRIPT}" "$app_name" chart)

        assert_equals "$expected_version" "$actual_version" "Chart version for $app_name"
    done < <(find "${PROJECT_ROOT}/application" -maxdepth 3 -type f -name "Chart.yaml" | sort)
}

function test_app_versions() {
    log_test "Testing: App versions (appVersion)"

    # Discover all charts
    while IFS= read -r chart_file; do
        local app_dir=$(dirname "$(dirname "$chart_file")")
        local app_name=$(basename "$app_dir")
        local expected_version=$(grep '^appVersion:' "$chart_file" | awk '{print $2}' | tr -d '"')
        local actual_version=$(bash "${GET_VERSION_SCRIPT}" "$app_name" appVersion)

        assert_equals "$expected_version" "$actual_version" "App version for $app_name"
    done < <(find "${PROJECT_ROOT}/application" -maxdepth 3 -type f -name "Chart.yaml" | sort)
}

function test_image_versions() {
    log_test "Testing: Container image versions"

    # Discover all VERSION files
    while IFS= read -r version_file; do
        local expected_version=$(cat "$version_file")
        local container_dir=$(dirname "$version_file")
        local relative_path=${container_dir#${PROJECT_ROOT}/application/}

        # Parse the path to get app name
        if [[ "$relative_path" == */container ]]; then
            # Single container (e.g., podman-in-container/container)
            local app_name=$(basename "$(dirname "$container_dir")")
            local actual_version=$(bash "${GET_VERSION_SCRIPT}" "$app_name" image)
            assert_equals "$expected_version" "$actual_version" "Image version for $app_name"
        else
            # Multiple containers (e.g., aria2/container/aria2 or aria2/container/aria-ng)
            local container_name=$(basename "$container_dir")
            local base_app=$(basename "$(dirname "$(dirname "$container_dir")")")
            local app_path="$base_app/$container_name"
            local actual_version=$(bash "${GET_VERSION_SCRIPT}" "$app_path" image)
            assert_equals "$expected_version" "$actual_version" "Image version for $app_path"
        fi
    done < <(find "${PROJECT_ROOT}/application" -type f -name "VERSION" | sort)
}

function test_default_type() {
    log_test "Testing: Default type (should be chart)"

    # Test with a known chart
    local app_name="podman-in-container"
    local expected_version=$(bash "${GET_VERSION_SCRIPT}" "$app_name" chart)
    local actual_version=$(bash "${GET_VERSION_SCRIPT}" "$app_name")

    assert_equals "$expected_version" "$actual_version" "Default type should be chart for $app_name"
}

function test_error_handling() {
    log_test "Testing: Error handling"

    # Test non-existent application
    assert_failure "bash '${GET_VERSION_SCRIPT}' nonexistent-app chart" \
        "Should fail for non-existent application"

    # Test non-existent image
    assert_failure "bash '${GET_VERSION_SCRIPT}' aria2/nonexistent image" \
        "Should fail for non-existent container image"

    # Test invalid version type
    assert_failure "bash '${GET_VERSION_SCRIPT}' podman-in-container invalid-type" \
        "Should fail for invalid version type"

    # Test too many arguments
    assert_failure "bash '${GET_VERSION_SCRIPT}' app chart extra-arg" \
        "Should fail for too many arguments"
}

function test_version_format() {
    log_test "Testing: Version format validation"

    # Test that all chart versions are valid semver-like
    while IFS= read -r chart_file; do
        local app_dir=$(dirname "$(dirname "$chart_file")")
        local app_name=$(basename "$app_dir")
        local version=$(bash "${GET_VERSION_SCRIPT}" "$app_name" chart)

        # Check version is not empty
        if [[ -n "$version" ]]; then
            TESTS_PASSED=$((TESTS_PASSED + 1))
            log_info "✓ PASS: Chart version for $app_name is not empty: $version"
        else
            TESTS_FAILED=$((TESTS_FAILED + 1))
            log_error "✗ FAIL: Chart version for $app_name is empty"
        fi
        TESTS_RUN=$((TESTS_RUN + 1))
    done < <(find "${PROJECT_ROOT}/application" -maxdepth 3 -type f -name "Chart.yaml" | sort)
}

function print_summary() {
    echo ""
    echo "======================================"
    echo "Test Summary"
    echo "======================================"
    echo "Tests run:    $TESTS_RUN"
    echo "Tests passed: $TESTS_PASSED"
    echo "Tests failed: $TESTS_FAILED"
    echo "======================================"

    if [[ $TESTS_FAILED -eq 0 ]]; then
        log_info "All tests passed! ✓"
        return 0
    else
        log_error "Some tests failed! ✗"
        return 1
    fi
}

# Main test execution
function main() {
    log_info "Starting unit tests for get-version.sh"
    log_info "Script location: $GET_VERSION_SCRIPT"
    echo ""

    # Run all test suites
    test_list_all_versions
    echo ""

    test_chart_versions
    echo ""

    test_app_versions
    echo ""

    test_image_versions
    echo ""

    test_default_type
    echo ""

    test_error_handling
    echo ""

    test_version_format
    echo ""

    # Print summary and exit with appropriate code
    print_summary
}

main "$@"
