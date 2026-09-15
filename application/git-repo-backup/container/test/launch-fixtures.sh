#!/usr/bin/env bash
# Launch a disposable TLS MinIO fixture for the git-repo-backup S3
# integration tests, and print the environment variables the tests expect.
# Usage (script works from any working directory):
#   launch-fixtures.sh start   # start fixture, print exports for the tests
#   launch-fixtures.sh stop    # stop and remove only what this script created
set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTAINER="git-repo-backup-it-$(date +%s)-$RANDOM"
STATE_ROOT="${TMPDIR:-/tmp}/${CONTAINER}"
PORT="${GIT_REPO_BACKUP_IT_PORT:-39000}"
MINIO_IMAGE="docker.io/minio/minio:latest"
MC_IMAGE="docker.io/minio/mc:latest"

log() { printf '%s\n' "$*" >&2; }

runtime() {
    if command -v podman >/dev/null 2>&1; then
        echo podman
    elif command -v docker >/dev/null 2>&1; then
        echo docker
    else
        log "neither podman nor docker is available"
        exit 1
    fi
}

stop() {
    local rt
    rt="$(runtime)"
    "${rt}" rm -f -t 3 "${CONTAINER}" >/dev/null 2>&1 || true
    rm -rf "${STATE_ROOT}"
    log "fixture stopped"
}

start() {
    local rt
    rt="$(runtime)"
    mkdir -p "${STATE_ROOT}/certs" "${STATE_ROOT}/data"

    # Self-signed CA plus a server certificate valid for localhost and
    # 127.0.0.1. The integration tests verify the backup binary refuses
    # this endpoint without the CA and succeeds with it.
    openssl req -x509 -newkey rsa:2048 -nodes -keyout "${STATE_ROOT}/certs/ca.key" \
        -out "${STATE_ROOT}/certs/ca.crt" -days 2 -subj "/CN=git-repo-backup-it-ca" >/dev/null 2>&1
    openssl req -newkey rsa:2048 -nodes -keyout "${STATE_ROOT}/certs/server.key" \
        -out "${STATE_ROOT}/certs/server.csr" -subj "/CN=localhost" >/dev/null 2>&1
    printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > "${STATE_ROOT}/certs/ext.cnf"
    openssl x509 -req -in "${STATE_ROOT}/certs/server.csr" -CA "${STATE_ROOT}/certs/ca.crt" \
        -CAkey "${STATE_ROOT}/certs/ca.key" -CAcreateserial -days 2 \
        -extfile "${STATE_ROOT}/certs/ext.cnf" -out "${STATE_ROOT}/certs/server.crt" >/dev/null 2>&1

    "${rt}" run -d --name "${CONTAINER}" \
        -p "127.0.0.1:${PORT}:9000" \
        -v "${STATE_ROOT}/certs:/certs:ro" \
        -v "${STATE_ROOT}/data:/data" \
        -e MINIO_ROOT_USER=gitrepobackup \
        -e MINIO_ROOT_PASSWORD=gitrepobackup-secret \
        "${MINIO_IMAGE}" server /data --certs-dir /certs >/dev/null

    # Wait for the TLS listener, then create the bucket. mc joins the
    # server's network namespace and talks TLS to localhost; --insecure is
    # fixture-only convenience, the backup binary itself always verifies.
    local i ready=0
    for i in $(seq 1 30); do
        if openssl s_client -connect "127.0.0.1:${PORT}" -servername localhost \
            -CAfile "${STATE_ROOT}/certs/ca.crt" </dev/null 2>/dev/null | \
            grep -q "Verify return code: 0"; then
            ready=1
            break
        fi
        sleep 1
    done
    if [ "${ready}" != 1 ]; then
        log "fixture did not become ready"
        stop
        exit 1
    fi
    "${rt}" run --rm --network "container:${CONTAINER}" "${MC_IMAGE}" \
        alias set it https://localhost:9000 gitrepobackup gitrepobackup-secret --insecure >/dev/null
    "${rt}" run --rm --network "container:${CONTAINER}" "${MC_IMAGE}" \
        mb --ignore-existing it/git-repo-backup-it --insecure >/dev/null

    cat <<ENV
export GIT_REPO_BACKUP_IT_S3_ENDPOINT="https://127.0.0.1:${PORT}"
export GIT_REPO_BACKUP_IT_S3_CA="${STATE_ROOT}/certs/ca.crt"
export GIT_REPO_BACKUP_IT_S3_ACCESS_KEY="gitrepobackup"
export GIT_REPO_BACKUP_IT_S3_SECRET_KEY="gitrepobackup-secret"
export GIT_REPO_BACKUP_IT_S3_BUCKET="git-repo-backup-it"
ENV
    log "fixture container: ${CONTAINER} (stop with: ${SCRIPT_DIR}/launch-fixtures.sh stop ${CONTAINER})"
    log "note: 'stop' removes the most recent fixture started by this script in this shell only when CONTAINER matches; to clean up manually: ${rt} rm -f ${CONTAINER}"
}

case "${1:-}" in
    start) start ;;
    stop)
        if [ "${2:-}" != "" ]; then CONTAINER="${2}"; STATE_ROOT="${TMPDIR:-/tmp}/${CONTAINER}"; fi
        stop
        ;;
    *) log "usage: $0 start|stop [container-name]"; exit 2 ;;
esac
