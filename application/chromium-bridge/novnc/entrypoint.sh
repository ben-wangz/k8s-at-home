#!/usr/bin/env bash
set -euo pipefail

export NOVNC_PORT="${NOVNC_PORT:-6080}"
export SERVER_HOST="${SERVER_HOST:-chromium-bridge-server}"
export SERVER_VNC_PORT="${SERVER_VNC_PORT:-5900}"

echo "chromium-bridge novnc started"
echo "listen: ${NOVNC_PORT}, target: ${SERVER_HOST}:${SERVER_VNC_PORT}"

exec /usr/share/novnc/utils/novnc_proxy --listen "${NOVNC_PORT}" --vnc "${SERVER_HOST}:${SERVER_VNC_PORT}"
