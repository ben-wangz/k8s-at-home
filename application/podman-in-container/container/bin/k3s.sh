#!/bin/bash
set -e

K3S_DATA_DIR="/var/lib/rancher/k3s"
K3S_PID_FILE="/var/run/k3s.pid"
K3S_LOG_FILE="/var/log/k3s.log"
export KUBECONFIG="/etc/rancher/k3s/k3s.yaml"

ensure_k3s() {
    if command -v k3s &>/dev/null; then
        return 0
    fi
    echo "k3s not found, installing..."
    curl -sfL https://rancher-mirror.rancher.cn/k3s/k3s-install.sh | \
        INSTALL_K3S_MIRROR=cn INSTALL_K3S_SKIP_START=true INSTALL_K3S_SKIP_ENABLE=true sh -
    echo "k3s installed successfully"
}

usage() {
    echo "Usage: $0 {start|stop|status|delete}"
    exit 1
}

cmd_start() {
    ensure_k3s
    if [ -f "$K3S_PID_FILE" ] && kill -0 "$(cat "$K3S_PID_FILE")" 2>/dev/null; then
        echo "k3s is already running (PID: $(cat "$K3S_PID_FILE"))"
        return 0
    fi
    echo "Starting k3s server..."
    nohup k3s server \
        --disable=traefik \
        --disable=servicelb \
        --snapshotter=native \
        --write-kubeconfig-mode=644 \
        > "$K3S_LOG_FILE" 2>&1 &
    echo $! > "$K3S_PID_FILE"
    echo "k3s started (PID: $(cat "$K3S_PID_FILE"))"
    echo "Waiting for k3s to be ready..."
    for i in {1..60}; do
        if k3s kubectl get nodes &>/dev/null; then
            echo "k3s is ready"
            return 0
        fi
        sleep 2
    done
    echo "Warning: k3s may not be fully ready, check: tail -f $K3S_LOG_FILE"
}

cmd_stop() {
    if [ ! -f "$K3S_PID_FILE" ]; then
        echo "k3s is not running (no PID file)"
        return 0
    fi
    local pid
    pid=$(cat "$K3S_PID_FILE")
    if kill -0 "$pid" 2>/dev/null; then
        echo "Stopping k3s (PID: $pid)..."
        kill "$pid"
        sleep 2
        kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$K3S_PID_FILE"
    echo "k3s stopped"
}

cmd_status() {
    if [ -f "$K3S_PID_FILE" ] && kill -0 "$(cat "$K3S_PID_FILE")" 2>/dev/null; then
        echo "k3s is running (PID: $(cat "$K3S_PID_FILE"))"
        k3s kubectl get nodes 2>/dev/null || echo "kubectl not ready yet"
    else
        echo "k3s is not running"
    fi
}

cmd_delete() {
    cmd_stop
    echo "Cleaning up k3s data..."
    rm -rf "$K3S_DATA_DIR"
    rm -f "$K3S_LOG_FILE"
    rm -rf /etc/rancher/k3s
    echo "k3s data deleted"
}

case "${1:-}" in
    start)  cmd_start ;;
    stop)   cmd_stop ;;
    status) cmd_status ;;
    delete) cmd_delete ;;
    *)      usage ;;
esac
