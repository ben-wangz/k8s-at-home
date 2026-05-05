#!/usr/bin/env bash
set -euo pipefail

export DISPLAY=:99
export SCREEN_RESOLUTION="${SCREEN_RESOLUTION:-1920x1080x24}"
export CDP_PORT="${CDP_PORT:-9222}"
export CHROME_INTERNAL_PORT="${CHROME_INTERNAL_PORT:-9223}"
export VNC_PORT="${VNC_PORT:-5900}"
export USER_DATA_DIR="${USER_DATA_DIR:-/tmp/chrome-user-data}"
export CHROME_BIN="${CHROME_BIN:-/opt/chrome/chrome-linux64/chrome}"
export STARTUP_TIMEOUT_SECONDS="${STARTUP_TIMEOUT_SECONDS:-30}"

mkdir -p "$USER_DATA_DIR"

pids=()

log() {
  printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

shutdown() {
  local code=$?
  for pid in "${pids[@]:-}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  wait || true
  exit "$code"
}

trap shutdown SIGTERM SIGINT EXIT

Xvfb :99 -screen 0 "$SCREEN_RESOLUTION" -ac +extension RANDR > /tmp/xvfb.log 2>&1 &
pids+=("$!")

fluxbox > /tmp/fluxbox.log 2>&1 &
pids+=("$!")

x11vnc -display :99 -forever -shared -rfbport "$VNC_PORT" -nopw -listen 0.0.0.0 > /tmp/x11vnc.log 2>&1 &
pids+=("$!")

if [ ! -x "$CHROME_BIN" ]; then
  echo "Chrome binary not found: $CHROME_BIN" > /tmp/chrome.log
  tail -f /tmp/chrome.log /tmp/xvfb.log /tmp/x11vnc.log /tmp/fluxbox.log
fi

touch /tmp/chrome.log

"$CHROME_BIN" \
  --no-sandbox \
  --disable-dev-shm-usage \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port="$CHROME_INTERNAL_PORT" \
  --user-data-dir="$USER_DATA_DIR" \
  --window-size=1920,1080 \
  --disable-gpu \
  --disable-features=TranslateUI \
  about:blank > /tmp/chrome.log 2>&1 &
pids+=("$!")

socat TCP-LISTEN:"$CDP_PORT",bind=0.0.0.0,reuseaddr,fork TCP:127.0.0.1:"$CHROME_INTERNAL_PORT" > /tmp/socat.log 2>&1 &
pids+=("$!")

log "chromium-bridge server booting"
log "CDP: $CDP_PORT (forward->127.0.0.1:$CHROME_INTERNAL_PORT), VNC: $VNC_PORT"

for _ in $(seq 1 "$STARTUP_TIMEOUT_SECONDS"); do
  if curl -fsS "http://127.0.0.1:${CHROME_INTERNAL_PORT}/json/version" >/dev/null 2>&1; then
    log "chrome devtools endpoint is ready"
    break
  fi
  sleep 1
done

if ! curl -fsS "http://127.0.0.1:${CHROME_INTERNAL_PORT}/json/version" >/dev/null 2>&1; then
  log "chrome devtools endpoint did not become ready in ${STARTUP_TIMEOUT_SECONDS}s"
  log "showing recent logs before exit"
  for f in /tmp/chrome.log /tmp/xvfb.log /tmp/x11vnc.log /tmp/fluxbox.log /tmp/socat.log; do
    if [ -f "$f" ]; then
      log "----- $f -----"
      tail -n 40 "$f" || true
    fi
  done
  exit 1
fi

tail -f /tmp/chrome.log /tmp/socat.log /tmp/xvfb.log /tmp/x11vnc.log /tmp/fluxbox.log
