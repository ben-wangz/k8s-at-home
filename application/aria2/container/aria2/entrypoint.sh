#! /bin/sh

set -e

DOWNLOAD_DIR=${DOWNLOAD_DIR:-/opt/aria2/downloads}
DHT_LISTEN_PORT=${DHT_LISTEN_PORT:-6868}
LISTEN_PORT=${LISTEN_PORT:-6868}
RPC_LISTEN_PORT=${RPC_LISTEN_PORT:-6800}
ARIA2_RUNTIME_DIR=${ARIA2_RUNTIME_DIR:-/opt/aria2/runtime}

exec aria2c --log=${ARIA2_RUNTIME_DIR}/aria2.log --log-level=error \
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
    --console-log-level=error \
    --disable-ipv6=true \
    --file-allocation=none \
    --no-conf=true \
    --save-session=${ARIA2_RUNTIME_DIR}/session \
    --save-session-interval=60
