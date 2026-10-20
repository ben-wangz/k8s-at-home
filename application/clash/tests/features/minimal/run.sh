#!/usr/bin/env bash
set -euo pipefail

CLASH_SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CLASH_TESTS_DIR="$(cd -- "$CLASH_SCRIPT_DIR/../.." && pwd)"
CLASH_APP_DIR="$(cd -- "$CLASH_TESTS_DIR/.." && pwd)"
CLASH_CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/clash/tests"
mkdir -p "$CLASH_CACHE_DIR"
chmod 700 "$CLASH_CACHE_DIR"
CLASH_TEST_BINARY="$(mktemp "$CLASH_CACHE_DIR/clash-tests.XXXXXX")"
trap 'rm -f "$CLASH_TEST_BINARY"' EXIT
GOWORK=off go -C "$CLASH_TESTS_DIR" build -trimpath -o "$CLASH_TEST_BINARY" ./cmd/clash-tests
export CLASH_TEST_APP_ROOT="$CLASH_APP_DIR"
"$CLASH_TEST_BINARY" "$@"
