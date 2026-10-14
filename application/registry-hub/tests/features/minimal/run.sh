#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
TESTS_DIR="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
REPO_ROOT="$(cd -- "$TESTS_DIR/../../.." && pwd)"
CACHE_BASE="${XDG_CACHE_HOME:-${HOME}/.cache}"
CACHE_DIR="$CACHE_BASE/registry-hub/tests"
BIN_PATH="$CACHE_DIR/registry-hub-tests"

mkdir -p "$CACHE_DIR"
chmod 700 "$CACHE_DIR"
BUILD_PATH="$(mktemp "$CACHE_DIR/.registry-hub-tests.XXXXXX")"
trap 'rm -f "$BUILD_PATH"' EXIT

GOWORK=off go -C "$TESTS_DIR" build -trimpath -o "$BUILD_PATH" ./cmd/registry-hub-tests
chmod 700 "$BUILD_PATH"
mv -f "$BUILD_PATH" "$BIN_PATH"
trap - EXIT

export REGISTRY_HUB_TEST_REPO_ROOT="$REPO_ROOT"
exec "$BIN_PATH" "$@"
