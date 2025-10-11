#!/bin/bash

set -o errexit -o nounset -o pipefail

# Unit tests for version bump utilities
# Pure unit tests without modifying actual files

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"

# Load dependency checking library
source "${SCRIPT_DIR}/lib/dependencies.sh"

# Check dependencies before loading library
NEEDS_PYYAML=true check_test_dependencies || exit 1

# Load version utilities library
source "${PROJECT_ROOT}/tools/lib/version-utils.sh"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

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
        log_error "✗ FAIL: $test_name (command should have succeeded)"
        return 1
    fi
}

function assert_failure() {
    local command="$1"
    local test_name="$2"

    TESTS_RUN=$((TESTS_RUN + 1))

    if eval "$command" &>/dev/null; then
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_error "✗ FAIL: $test_name (command should have failed)"
        return 1
    else
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_info "✓ PASS: $test_name"
        return 0
    fi
}

# Test semver parsing
function test_parse_semver() {
    log_test "Testing parse_semver function"

    local test_cases=(
        "1.2.3|1 2 3"
        "0.0.0|0 0 0"
        "10.20.30|10 20 30"
        "1.0.0|1 0 0"
        "0.1.0|0 1 0"
        "0.0.1|0 0 1"
    )

    for test_case in "${test_cases[@]}"; do
        IFS='|' read -r input expected <<< "$test_case"
        result=$(parse_semver "$input")
        assert_equals "$expected" "$result" "parse_semver($input)"
    done

    # Test invalid formats
    assert_failure "parse_semver '1.2'" "parse_semver should reject '1.2'"
    assert_failure "parse_semver '1.2.3.4'" "parse_semver should reject '1.2.3.4'"
    assert_failure "parse_semver 'a.b.c'" "parse_semver should reject 'a.b.c'"
}

# Test version bumping
function test_bump_semver() {
    log_test "Testing bump_semver function"

    local test_cases=(
        # format: "input|bump_type|expected"
        "1.0.0|patch|1.0.1"
        "1.0.0|minor|1.1.0"
        "1.0.0|major|2.0.0"
        "1.2.3|patch|1.2.4"
        "1.2.3|minor|1.3.0"
        "1.2.3|major|2.0.0"
        "0.0.0|patch|0.0.1"
        "0.0.0|minor|0.1.0"
        "0.0.0|major|1.0.0"
        "0.9.9|patch|0.9.10"
        "0.9.9|minor|0.10.0"
        "9.9.9|major|10.0.0"
        "1.0.99|patch|1.0.100"
        "1.99.0|minor|1.100.0"
        "99.0.0|major|100.0.0"
    )

    for test_case in "${test_cases[@]}"; do
        IFS='|' read -r input bump_type expected <<< "$test_case"
        result=$(bump_semver "$input" "$bump_type")
        assert_equals "$expected" "$result" "bump_semver($input, $bump_type)"
    done

    # Test invalid bump types
    assert_failure "bump_semver '1.0.0' 'invalid'" "bump_semver should reject invalid bump type"
}

# Test path resolution for VERSION files
function test_resolve_version_file_path() {
    log_test "Testing resolve_version_file_path function"

    local base_dir="${PROJECT_ROOT}/application"
    local test_cases=(
        # format: "app_path|expected_suffix"
        "podman-in-container|podman-in-container/container/VERSION"
        "aria2/aria2|aria2/container/aria2/VERSION"
        "aria2/aria-ng|aria2/container/aria-ng/VERSION"
    )

    for test_case in "${test_cases[@]}"; do
        IFS='|' read -r app_path expected_suffix <<< "$test_case"
        result=$(resolve_version_file_path "$app_path" "$base_dir")
        expected="${base_dir}/${expected_suffix}"
        assert_equals "$expected" "$result" "resolve_version_file_path($app_path)"
    done
}

# Test path resolution for Chart.yaml files
function test_resolve_chart_file_path() {
    log_test "Testing resolve_chart_file_path function"

    local base_dir="${PROJECT_ROOT}/application"
    local test_cases=(
        # format: "chart_name|expected_suffix"
        "podman-in-container|podman-in-container/chart/Chart.yaml"
        "aria2|aria2/chart/Chart.yaml"
        "clash|clash/chart/Chart.yaml"
    )

    for test_case in "${test_cases[@]}"; do
        IFS='|' read -r chart_name expected_suffix <<< "$test_case"
        result=$(resolve_chart_file_path "$chart_name" "$base_dir")
        expected="${base_dir}/${expected_suffix}"
        assert_equals "$expected" "$result" "resolve_chart_file_path($chart_name)"
    done
}

# Test metadata extraction from Chart.yaml
function test_extract_images_metadata() {
    log_test "Testing extract_images_metadata function"

    # Test aria2 (should have 2 images)
    local aria2_chart="${PROJECT_ROOT}/application/aria2/chart/Chart.yaml"
    if [[ -f "$aria2_chart" ]]; then
        local aria2_images=$(extract_images_metadata "$aria2_chart")
        local aria2_count=$(echo "$aria2_images" | grep -c '^' || true)

        assert_equals "2" "$aria2_count" "aria2 should have 2 images in annotations"

        # Check if specific images are present
        if echo "$aria2_images" | grep -q "aria2|aria2/aria2|aria2.image.tag"; then
            TESTS_PASSED=$((TESTS_PASSED + 1))
            log_info "✓ PASS: aria2 metadata contains aria2 image"
        else
            TESTS_FAILED=$((TESTS_FAILED + 1))
            log_error "✗ FAIL: aria2 metadata missing aria2 image"
        fi
        TESTS_RUN=$((TESTS_RUN + 1))

        if echo "$aria2_images" | grep -q "aria-ng|aria2/aria-ng|ariaNg.image.tag"; then
            TESTS_PASSED=$((TESTS_PASSED + 1))
            log_info "✓ PASS: aria2 metadata contains aria-ng image"
        else
            TESTS_FAILED=$((TESTS_FAILED + 1))
            log_error "✗ FAIL: aria2 metadata missing aria-ng image"
        fi
        TESTS_RUN=$((TESTS_RUN + 1))
    else
        log_error "aria2 Chart.yaml not found, skipping metadata tests"
    fi

    # Test podman-in-container (should have 1 image)
    local podman_chart="${PROJECT_ROOT}/application/podman-in-container/chart/Chart.yaml"
    if [[ -f "$podman_chart" ]]; then
        local podman_images=$(extract_images_metadata "$podman_chart")
        local podman_count=$(echo "$podman_images" | grep -c '^' || true)

        assert_equals "1" "$podman_count" "podman-in-container should have 1 image in annotations"

        if echo "$podman_images" | grep -q "podman-in-container|podman-in-container|image.tag"; then
            TESTS_PASSED=$((TESTS_PASSED + 1))
            log_info "✓ PASS: podman-in-container metadata contains image"
        else
            TESTS_FAILED=$((TESTS_FAILED + 1))
            log_error "✗ FAIL: podman-in-container metadata missing image"
        fi
        TESTS_RUN=$((TESTS_RUN + 1))
    else
        log_error "podman-in-container Chart.yaml not found, skipping metadata tests"
    fi
}

# Test YAML value reading
function test_read_yaml_value() {
    log_test "Testing read_yaml_value function"

    local aria2_chart="${PROJECT_ROOT}/application/aria2/chart/Chart.yaml"
    if [[ -f "$aria2_chart" ]]; then
        # Test reading simple values
        local name=$(read_yaml_value "$aria2_chart" "name")
        assert_equals "aria2" "$name" "read Chart name"

        local api_version=$(read_yaml_value "$aria2_chart" "apiVersion")
        assert_equals "v2" "$api_version" "read Chart apiVersion"

        # Version field is tested but value is not hardcoded
        assert_success "read_yaml_value '$aria2_chart' 'version'" "read Chart version"
        assert_success "read_yaml_value '$aria2_chart' 'appVersion'" "read Chart appVersion"
    else
        log_error "aria2 Chart.yaml not found, skipping YAML read tests"
    fi
}

# Test file validation
function test_validate_file_exists() {
    log_test "Testing validate_file_exists function"

    # Test with existing file
    local existing_file="${PROJECT_ROOT}/tools/lib/version-utils.sh"
    assert_success "validate_file_exists '$existing_file' 'test'" "validate existing file"

    # Test with non-existing file
    local nonexistent_file="/tmp/nonexistent_file_12345.txt"
    assert_failure "validate_file_exists '$nonexistent_file' 'test'" "validate non-existing file"
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
    log_info "Starting unit tests for version bump utilities"
    log_info "Library: ${PROJECT_ROOT}/tools/lib/version-utils.sh"
    echo ""

    # Run all test suites
    test_parse_semver
    echo ""

    test_bump_semver
    echo ""

    test_resolve_version_file_path
    echo ""

    test_resolve_chart_file_path
    echo ""

    test_extract_images_metadata
    echo ""

    test_read_yaml_value
    echo ""

    test_validate_file_exists
    echo ""

    # Print summary and exit with appropriate code
    print_summary
}

main "$@"
