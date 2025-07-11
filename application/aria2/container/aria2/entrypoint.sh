#! /bin/sh

set -e

DOWNLOAD_DIR=${DOWNLOAD_DIR:-/opt/aria2/downloads}
DHT_LISTEN_PORT=${DHT_LISTEN_PORT:-6868}
LISTEN_PORT=${LISTEN_PORT:-6868}
RPC_LISTEN_PORT=${RPC_LISTEN_PORT:-6800}
ARIA2_RUNTIME_DIR=${ARIA2_RUNTIME_DIR:-/opt/aria2/runtime}
DEBUG=${DEBUG:-false}
RPC_SECRET=${RPC_SECRET:-}
if [ -n "$RPC_SECRET" ]; then
    RPC_SECRET_OPTION="--rpc-secret=${RPC_SECRET}"
else
    RPC_SECRET_OPTION=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 16)
    echo "WARN: RPC_SECRET not set, use random value: ${RPC_SECRET_OPTION}"
fi

if [ "$DEBUG" = true ]; then
    echo "DEBUG: Enabling debug mode"
    LOG_LEVEL=debug
    set -x
else
    LOG_LEVEL=info
fi
aria2c --log=${ARIA2_RUNTIME_DIR}/aria2.log --log-level=${LOG_LEVEL} \
    --dir=${DOWNLOAD_DIR} \
    --bt-detach-seed-only=true \
    --bt-force-encryption=true \
    --bt-remove-unselected-file=true \
    --dht-file-path=${ARIA2_RUNTIME_DIR}/dht.dat \
    --dht-listen-port=${DHT_LISTEN_PORT} \
    --listen-port=${LISTEN_PORT} \
    --enable-rpc=true \
    --rpc-listen-port=${RPC_LISTEN_PORT} \
    --rpc-save-upload-metadata=false \
    ${RPC_SECRET_OPTION} \
    --daemon=true \
    --console-log-level=error \
    --disable-ipv6=true \
    --file-allocation=none \
    --no-conf=true \
    --save-session=${ARIA2_RUNTIME_DIR}/session \
    --save-session-interval=60 \
    --continue=true

tail -f ${ARIA2_RUNTIME_DIR}/aria2.log
