#!/bin/sh

set -eu

script_dir="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
container_dir="$(dirname "$script_dir")"
render_script="$container_dir/bin/cli-proxy-api-render-config"
login_script="$container_dir/bin/cli-proxy-api-xai-login"
tmp_dir="$(mktemp -d)"

cleanup() {
    rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$tmp_dir/input/config" "$tmp_dir/input/credentials" "$tmp_dir/runtime" "$tmp_dir/auths"

cat > "$tmp_dir/input/config/config-base.json" <<'EOF'
{
  "host": "",
  "port": 8317,
  "auth-dir": "/data/auths",
  "api-keys": []
}
EOF
printf '%s\n' '["first-key","second-key"]' > "$tmp_dir/input/credentials/api-keys"

CLI_PROXY_CONFIG_BASE_PATH="$tmp_dir/input/config/config-base.json" \
CLI_PROXY_API_KEYS_PATH="$tmp_dir/input/credentials/api-keys" \
CLI_PROXY_CONFIG_OUTPUT_PATH="$tmp_dir/runtime/config.yaml" \
    "$render_script"

jq -e '.port == 8317 and ."api-keys" == ["first-key", "second-key"]' \
    "$tmp_dir/runtime/config.yaml" >/dev/null

printf '%s\n' '[]' > "$tmp_dir/input/credentials/api-keys"
if CLI_PROXY_CONFIG_BASE_PATH="$tmp_dir/input/config/config-base.json" \
    CLI_PROXY_API_KEYS_PATH="$tmp_dir/input/credentials/api-keys" \
    CLI_PROXY_CONFIG_OUTPUT_PATH="$tmp_dir/runtime/invalid.yaml" \
    "$render_script" >/dev/null 2>&1; then
    echo "renderer accepted an empty api-keys array" >&2
    exit 1
fi

cat > "$tmp_dir/mock-cli" <<EOF
#!/bin/sh
set -eu
cat > "$tmp_dir/auths/xai-test.json" <<'JSON'
{"type":"xai","auth_kind":"oauth","access_token":"access","refresh_token":"refresh","using_api":false}
JSON
EOF
chmod 0755 "$tmp_dir/mock-cli"

CLI_PROXY_BINARY="$tmp_dir/mock-cli" \
CLI_PROXY_CONFIG_PATH="$tmp_dir/runtime/config.yaml" \
CLI_PROXY_AUTH_DIR="$tmp_dir/auths" \
XAI_USING_API=true \
    "$login_script" >/dev/null

jq -e '.using_api == true' "$tmp_dir/auths/xai-test.json" >/dev/null

CLI_PROXY_BINARY="$tmp_dir/mock-cli" \
CLI_PROXY_CONFIG_PATH="$tmp_dir/runtime/config.yaml" \
CLI_PROXY_AUTH_DIR="$tmp_dir/auths" \
XAI_USING_API=false \
    "$login_script" >/dev/null

jq -e '.using_api == false' "$tmp_dir/auths/xai-test.json" >/dev/null

cat > "$tmp_dir/mock-cli-noop" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$tmp_dir/mock-cli-noop"

if CLI_PROXY_BINARY="$tmp_dir/mock-cli-noop" \
    CLI_PROXY_CONFIG_PATH="$tmp_dir/runtime/config.yaml" \
    CLI_PROXY_AUTH_DIR="$tmp_dir/auths" \
    XAI_USING_API=true \
    "$login_script" >/dev/null 2>&1; then
    echo "xAI login accepted an unchanged authentication file" >&2
    exit 1
fi

echo "CLIProxyAPI container script tests passed"
