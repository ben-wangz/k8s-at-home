#!/usr/bin/env bash
# Fixed subcommands executed inside the integration test container.
# This script never runs Podman and never touches the host.
#
#   in-container.sh gen-certs     write fixture CA + server certs to the
#                                 mounted volumes (/ca-out, /certs-out)
#   in-container.sh run-tests MODE  preflight, discover tests, run the Go
#                                 integration suite, write /results
#                                 (MODE: local|all)
#
# Exit code of run-tests is the preserved `go test` exit code; the checker
# verdict is recorded separately in /results/checker-exit-code.
set -o nounset -o pipefail

RESULTS=/results
TOOLDIR=/opt/git-repo-backup-test
SRC=/src
SSH_DIR=/etc/git-repo-backup/ssh

log() { printf '%s\n' "$*" >&2; }
die() { log "in-container: error: $*"; exit 1; }
usage_die() { log "usage: in-container.sh gen-certs|run-tests <local|all>"; exit 2; }

record_tool_versions() {
    {
        printf 'go: %s\n' "$(go version 2>&1 || echo unavailable)"
        printf 'git: %s\n' "$(git --version 2>&1 || echo unavailable)"
        printf 'ssh: %s\n' "$(ssh -V 2>&1 | head -1 || echo unavailable)"
        printf 'image_uid: %s\n' "$(id -u)"
    } >"$RESULTS/tool-versions.txt" 2>/dev/null || true
}

preflight() {
    local mode="$1" var
    [ "$(id -u)" -eq 0 ] || die "must run as container uid 0"
    for bin in go git /usr/sbin/sshd ssh ssh-keygen tar gzip sha256sum python3 openssl curl; do
        command -v "$bin" >/dev/null 2>&1 || die "missing required tool: $bin"
    done
    [ -d "$SSH_DIR" ] || die "fixed ssh directory missing: $SSH_DIR"
    ( touch "$SSH_DIR/.writable-check" && rm -f "$SSH_DIR/.writable-check" ) \
        || die "fixed ssh directory not writable: $SSH_DIR"
    mkdir -p "$RESULTS" || die "cannot create $RESULTS"
    if [ "$mode" = "all" ]; then
        for var in \
            GIT_REPO_BACKUP_IT_S3_ENDPOINT \
            GIT_REPO_BACKUP_IT_S3_CA \
            GIT_REPO_BACKUP_IT_S3_ACCESS_KEY \
            GIT_REPO_BACKUP_IT_S3_SECRET_KEY \
            GIT_REPO_BACKUP_IT_S3_BUCKET; do
            [ -n "${!var:-}" ] || die "missing environment variable: $var"
        done
        [ -r "${GIT_REPO_BACKUP_IT_S3_CA:?}" ] || die "fixture CA unreadable"
    fi
}

gen_certs() {
    # Fixture CA and server certificate for the in-pod MinIO. The CA
    # private key lives only in this container's temporary directory and
    # is destroyed when the container is removed.
    local tmp
    tmp=$(mktemp -d) || die "mktemp failed"
    [ -d /ca-out ] && [ -d /certs-out ] || die "cert output volumes not mounted"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$tmp/ca.key" -out /ca-out/ca.crt \
        -days 2 -subj "/CN=git-repo-backup-it-ca" >/dev/null 2>&1 || die "CA generation failed"
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$tmp/server.key" -out "$tmp/server.csr" \
        -subj "/CN=localhost" >/dev/null 2>&1 || die "server CSR failed"
    printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' >"$tmp/ext.cnf"
    openssl x509 -req -in "$tmp/server.csr" \
        -CA /ca-out/ca.crt -CAkey "$tmp/ca.key" -CAcreateserial -days 2 \
        -extfile "$tmp/ext.cnf" -out /certs-out/public.crt >/dev/null 2>&1 \
        || die "server certificate signing failed"
    cp "$tmp/server.key" /certs-out/private.key || die "server key copy failed"
    chmod 0644 /ca-out/ca.crt /certs-out/public.crt
    chmod 0600 /certs-out/private.key
    rm -rf "$tmp"
    log "fixture certificates generated"
}

run_tests() {
    local mode="$1" run_pattern list_pattern go_exit
    case "$mode" in
        local) run_pattern='^TestLocal' ; list_pattern='^TestLocal' ;;
        all) run_pattern='' ; list_pattern='.' ;;
        *) usage_die ;;
    esac
    preflight "$mode"
    record_tool_versions

    # Expected top-level tests are discovered from the current source with
    # the same tags/pattern as the run, so partial or truncated runs cannot
    # be mistaken for success.
    go -C "$SRC" test -tags integration -list "$list_pattern" ./integration/ \
        >"$RESULTS/test-list.txt" 2>"$RESULTS/list-stderr.log" \
        || die "test discovery (go test -list) failed"

    local cmd=(go -C "$SRC" test -tags integration -count=1 -parallel=1 -timeout=30m -json)
    if [ -n "$run_pattern" ]; then
        cmd+=(-run "$run_pattern")
    fi
    cmd+=(./integration/)

    "${cmd[@]}" 2> >(tee "$RESULTS/test-stderr.log" >&2) | tee "$RESULTS/test-events.jsonl"
    go_exit=${PIPESTATUS[0]}
    printf '%s\n' "$go_exit" >"$RESULTS/go-exit-code"

    python3 "$TOOLDIR/check-results.py" \
        --mode "$mode" \
        --list "$RESULTS/test-list.txt" \
        --events "$RESULTS/test-events.jsonl" \
        --go-exit "$go_exit" \
        --output "$RESULTS/result.json"
    printf '%s\n' "$?" >"$RESULTS/checker-exit-code"
    log "go exit=$go_exit checker exit=$(cat "$RESULTS/checker-exit-code" 2>/dev/null || echo unavailable)"
    exit "$go_exit"
}

case "${1:-}" in
    gen-certs) gen_certs ;;
    run-tests) [ $# -eq 2 ] || usage_die ; run_tests "$2" ;;
    *) usage_die ;;
esac
