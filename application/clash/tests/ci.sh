#!/usr/bin/env bash
set -Eeuo pipefail

test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
app_dir="$(cd -- "$test_dir/.." && pwd)"
repo_root="$(git -C "$app_dir" rev-parse --show-toplevel)"
app_relative_dir="${app_dir#"$repo_root"/}"
frontend_dir="$app_dir/console/frontend"
browser_dir="$test_dir/browser"
chart_dir="$app_dir/chart"

npm --prefix "$frontend_dir" ci
npm --prefix "$frontend_dir" run build
git -C "$repo_root" diff --exit-code -- "$app_relative_dir/chart/files/console"

npm --prefix "$browser_dir" ci
npm --prefix "$browser_dir" exec -- playwright install --with-deps chromium
helm dependency update "$chart_dir"

go -C "$test_dir" test -race ./...
"$test_dir/features/minimal/run.sh" run --mock
"$test_dir/features/minimal/run.sh" run --mock --scenario feature-controller-auth
"$test_dir/features/minimal/run.sh" run --mock --scenario feature-controller-failure
