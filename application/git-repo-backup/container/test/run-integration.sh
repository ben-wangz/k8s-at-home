#!/usr/bin/env bash
# git-repo-backup integration test entry point (rootless Podman).
#
#   run-integration.sh local [--output-dir ABS_PATH]
#   run-integration.sh all --minio-image REF --mc-image REF [--output-dir ABS_PATH]
#   run-integration.sh --help
#
# The suite runs inside a disposable pod: the test container uses uid 0 in
# container space (mapped to the unprivileged host user), an in-pod TLS
# MinIO serves the S3 fixture on the pod loopback, and no host ports,
# sockets, or privileged flags are used. stdout carries only `go test`
# JSON lines; everything else goes to stderr and the evidence directory.
set -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# container/test -> git-repo-backup -> application -> repository root.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
# shellcheck source=launch-fixtures.sh
source "${SCRIPT_DIR}/launch-fixtures.sh"

MODE=""
MINIO_IMAGE=""
MC_IMAGE=""
OUTPUT_DIR=""
RUN_ID="gitrb-$(date -u +%Y%m%dT%H%M%SZ)-${RANDOM}"
LABEL="git-repo-backup-it-run=${RUN_ID}"
POD="${RUN_ID}-pod"
RUNNER="${RUN_ID}-runner"
MINIO_NAME="${RUN_ID}-minio"
CA_VOL="${RUN_ID}-ca"
CERT_VOL="${RUN_ID}-certs"
DATA_VOL="${RUN_ID}-data"
IMAGE_TAG="localhost/git-repo-backup-integration:${RUN_ID}"
BUCKET="git-repo-backup-it"
OUT="${REPO_ROOT}/build/git-repo-backup-integration/${RUN_ID}"
IMAGE_ID="unavailable"
IMAGE_PREEXISTING=0
MINIO_IMAGE_ID="unavailable"
MC_IMAGE_ID="unavailable"
RUNNER_EXIT="unavailable"
SIGNALLED=0
CLEANUP_FAILED=0
FINAL_EXIT=""
EXIT_SOURCE=""
CREATED_VOLUMES=()

usage() {
    cat <<'USAGE'
usage: run-integration.sh local [--output-dir ABS_PATH]
       run-integration.sh all --minio-image REF --mc-image REF [--output-dir ABS_PATH]
       run-integration.sh --help

Runs the git-repo-backup integration suite in a disposable rootless-Podman
pod. `local` runs only TestLocal* without S3 resources; `all` also starts
an in-pod TLS MinIO (explicit release tag or digest required; `latest` and
untagged refs are rejected). Evidence is kept in the output directory.
USAGE
}

err() { printf '%s\n' "$*" >&2; }
die_usage() { err "usage error: $*"; err; usage >&2; exit 2; }
info() { printf 'run-integration: %s\n' "$*" >&2; }

write_run_info() { printf '%s\n' "$*" >>"${OUT}/run-info.txt"; }

add_resource() { printf '%s %s\n' "$1" "$2" >>"${OUT}/resources.txt"; }

# fail_infra reports an infrastructure failure, collects whatever evidence
# exists, cleans up partially created resources, and exits 1. Business test
# failures never take this path.
fail_infra() {
    err "infrastructure failure: $*"
    collect_evidence
    EXIT_SOURCE="infrastructure"
    write_run_info "exit_source=${EXIT_SOURCE}"
    cleanup_resources
    exit 1
}

valid_image_ref() {
    local ref="$1" name="${1##*/}"
    case "$ref" in
        *latest*) return 1 ;;
    esac
    if [[ "$ref" == *"@sha256:"* ]]; then
        [[ "$ref" =~ ^[a-z0-9./:_-]+@sha256:[0-9a-f]{64}$ ]]
        return
    fi
    [[ "$name" == *:* ]]
}

parse_args() {
    local saw_minio=0 saw_mc=0 saw_out=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --help) usage; exit 0 ;;
            local|all)
                [ -z "$MODE" ] || die_usage "mode given twice"
                MODE="$1"; shift ;;
            --minio-image)
                [ "$saw_minio" -eq 0 ] || die_usage "--minio-image given twice"
                saw_minio=1; shift
                [ $# -gt 0 ] || die_usage "--minio-image requires a value"
                MINIO_IMAGE="$1"; shift ;;
            --mc-image)
                [ "$saw_mc" -eq 0 ] || die_usage "--mc-image given twice"
                saw_mc=1; shift
                [ $# -gt 0 ] || die_usage "--mc-image requires a value"
                MC_IMAGE="$1"; shift ;;
            --output-dir)
                [ "$saw_out" -eq 0 ] || die_usage "--output-dir given twice"
                saw_out=1; shift
                [ $# -gt 0 ] || die_usage "--output-dir requires a value"
                OUTPUT_DIR="$1"; shift ;;
            *) die_usage "unknown argument: $1" ;;
        esac
    done
    [ -n "$MODE" ] || die_usage "mode is required (local|all)"
    if [ "$MODE" = "local" ]; then
        [ "$saw_minio" -eq 0 ] && [ "$saw_mc" -eq 0 ] \
            || die_usage "--minio-image/--mc-image apply only to mode all"
    else
        [ "$saw_minio" -eq 1 ] || die_usage "mode all requires --minio-image"
        [ "$saw_mc" -eq 1 ] || die_usage "mode all requires --mc-image"
        valid_image_ref "$MINIO_IMAGE" || die_usage "invalid --minio-image ref (explicit non-latest tag or digest required): $MINIO_IMAGE"
        valid_image_ref "$MC_IMAGE" || die_usage "invalid --mc-image ref (explicit non-latest tag or digest required): $MC_IMAGE"
    fi
    if [ -n "$OUTPUT_DIR" ]; then
        case "$OUTPUT_DIR" in
            /*) ;;
            *) die_usage "--output-dir must be an absolute path" ;;
        esac
        if [ -e "$OUTPUT_DIR" ] && [ -n "$(ls -A "$OUTPUT_DIR" 2>/dev/null)" ]; then
            die_usage "--output-dir must be a new or empty directory: $OUTPUT_DIR"
        fi
        OUT="$OUTPUT_DIR"
    fi
}

preflight() {
    [ "$(uname -s)" = "Linux" ] || { err "infrastructure failure: Linux host required"; exit 1; }
    command -v podman >/dev/null 2>&1 || { err "infrastructure failure: podman not found"; exit 1; }
    local rootless
    rootless="$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" \
        || { err "infrastructure failure: podman info failed"; exit 1; }
    [ "$rootless" = "true" ] || { err "infrastructure failure: rootless podman required (no sudo fallback)"; exit 1; }
}

create_evidence() {
    mkdir -p "$OUT" || { err "infrastructure failure: cannot create evidence dir $OUT"; exit 1; }
    : >"${OUT}/resources.txt"
    write_run_info "mode=${MODE}"
    write_run_info "run_id=${RUN_ID}"
    write_run_info "repo_root=${REPO_ROOT}"
    write_run_info "output_dir=${OUT}"
    write_run_info "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if git -C "$REPO_ROOT" rev-parse HEAD >/dev/null 2>&1; then
        write_run_info "revision=$(git -C "$REPO_ROOT" rev-parse HEAD)"
        write_run_info "dirty_entries=$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
    else
        write_run_info "revision=unavailable"
        write_run_info "dirty_entries=unavailable"
    fi
}

build_image() {
    info "building test image (context: repository root)"
    if [ "$IMAGE_PREEXISTING" -eq 0 ] && podman image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
        IMAGE_PREEXISTING=1
        fail_infra "test image tag already exists: $IMAGE_TAG"
    fi
    local -a build_args=(
        --file "${REPO_ROOT}/application/git-repo-backup/container/test/Containerfile"
        --tag "$IMAGE_TAG"
        --label "$LABEL"
    )
    # Optional builder-image override for environments where the pinned
    # default registry is unreachable (e.g. docker.io blocked); any
    # replacement must still satisfy the src/go.mod toolchain baseline.
    if [ -n "${GIT_REPO_BACKUP_IT_GO_IMAGE:-}" ]; then
        build_args+=(--build-arg "GO_IMAGE=${GIT_REPO_BACKUP_IT_GO_IMAGE}")
        write_run_info "go_image_override=${GIT_REPO_BACKUP_IT_GO_IMAGE}"
    fi
    # Optional Go module proxy override for restricted-egress environments.
    if [ -n "${GIT_REPO_BACKUP_IT_GOPROXY:-}" ]; then
        build_args+=(--build-arg "GOPROXY=${GIT_REPO_BACKUP_IT_GOPROXY}")
        write_run_info "goproxy_override=${GIT_REPO_BACKUP_IT_GOPROXY}"
    fi
    build_args+=("$REPO_ROOT")
    podman build "${build_args[@]}" >"${OUT}/build.log" 2>&1 \
        || fail_infra "test image build failed (see build.log)"
    IMAGE_ID="$(podman image inspect "$IMAGE_TAG" --format '{{.Id}}' 2>/dev/null || echo unavailable)"
    write_run_info "test_image=${IMAGE_TAG}"
    write_run_info "test_image_id=${IMAGE_ID}"
    add_resource image "$IMAGE_TAG"
}

create_pod() {
    local -a pod_args=(--name "$POD" --label "$LABEL")
    # Podman normally selects its configured infra image. An explicit image
    # override is useful on installations with a restricted infra registry,
    # but a host-side catatonit binary is not required.
    if [ -n "${GIT_REPO_BACKUP_IT_PAUSE_IMAGE:-}" ]; then
        pod_args+=(--infra-image "${GIT_REPO_BACKUP_IT_PAUSE_IMAGE}")
        if [ -n "${GIT_REPO_BACKUP_IT_PAUSE_COMMAND:-}" ]; then
            pod_args+=(--infra-command "${GIT_REPO_BACKUP_IT_PAUSE_COMMAND}")
        fi
        write_run_info "pause_image=${GIT_REPO_BACKUP_IT_PAUSE_IMAGE}"
        write_run_info "pause_command=${GIT_REPO_BACKUP_IT_PAUSE_COMMAND:-<image default>}"
    fi
    podman pod create "${pod_args[@]}" >/dev/null 2>&1 \
        || fail_infra "pod create failed"
    add_resource pod "$POD"
}

prepare_fixture() {
    local vol
    for vol in "$CA_VOL" "$CERT_VOL" "$DATA_VOL"; do
        if podman volume inspect "$vol" >/dev/null 2>&1; then
            fail_infra "fixture volume already exists: $vol"
        fi
        podman volume create --label "$LABEL" "$vol" >/dev/null 2>&1 \
            || fail_infra "volume create failed: $vol"
        CREATED_VOLUMES+=("$vol")
        add_resource volume "$vol"
    done
    info "generating fixture certificates"
    grb_gen_certs "$IMAGE_TAG" "$CA_VOL" "$CERT_VOL" \
        || fail_infra "certificate generation failed"
    info "starting TLS MinIO (${MINIO_IMAGE})"
    grb_start_minio "$MINIO_IMAGE" "$POD" "$MINIO_NAME" "$LABEL" "$CERT_VOL" "$DATA_VOL" >/dev/null \
        || fail_infra "MinIO start failed"
    add_resource container "$MINIO_NAME"
    MINIO_IMAGE_ID="$(podman inspect -f '{{.Image}}' "$MINIO_NAME" 2>/dev/null || echo unavailable)"
    write_run_info "minio_image=${MINIO_IMAGE}"
    write_run_info "minio_image_id=${MINIO_IMAGE_ID}"
    write_run_info "mc_image=${MC_IMAGE}"
    MC_IMAGE_ID="$(podman image inspect "$MC_IMAGE" --format '{{.Id}}' 2>/dev/null || echo unavailable)"
    write_run_info "mc_image_id=${MC_IMAGE_ID}"
    info "waiting for MinIO readiness (max 60s)"
    grb_minio_ready "$IMAGE_TAG" "$POD" "$CA_VOL" \
        || { grb_minio_logs "$MINIO_NAME" "${OUT}/minio.log"
             fail_infra "MinIO readiness failed (see minio.log)"; }
    info "running S3 precheck (bucket create/read/delete with trusted CA)"
    grb_s3_precheck "$MC_IMAGE" "$POD" "$CA_VOL" "$BUCKET" "$LABEL" \
        || { grb_minio_logs "$MINIO_NAME" "${OUT}/minio.log"
             fail_infra "S3 precheck failed (see minio.log)"; }
}

start_runner() {
    local -a args=(run -d --pod "$POD" --name "$RUNNER" --label "$LABEL")
    if [ "$MODE" = "all" ]; then
        args+=(
            -v "${CA_VOL}:/fixture-ca:ro"
            -e GIT_REPO_BACKUP_IT_S3_ENDPOINT=https://127.0.0.1:9000
            -e GIT_REPO_BACKUP_IT_S3_CA=/fixture-ca/ca.crt
            -e GIT_REPO_BACKUP_IT_S3_ACCESS_KEY=gitrepobackup
            -e GIT_REPO_BACKUP_IT_S3_SECRET_KEY=gitrepobackup-secret
            -e GIT_REPO_BACKUP_IT_S3_BUCKET=${BUCKET}
        )
    fi
    args+=("$IMAGE_TAG" /opt/git-repo-backup-test/in-container.sh run-tests "$MODE")
    podman "${args[@]}" >/dev/null 2>&1 \
        || fail_infra "runner start failed"
    add_resource container "$RUNNER"
}

wait_runner() {
    local status
    while :; do
        status="$(podman inspect -f '{{.State.Status}}' "$RUNNER" 2>/dev/null || echo missing)"
        case "$status" in
            running|paused|created) sleep 2 ;;
            *) break ;;
        esac
    done
    RUNNER_EXIT="$(podman inspect -f '{{.State.ExitCode}}' "$RUNNER" 2>/dev/null || echo unavailable)"
    write_run_info "runner_exit=${RUNNER_EXIT}"
}

collect_evidence() {
    if [ "$MODE" = "all" ]; then
        grb_minio_logs "$MINIO_NAME" "${OUT}/minio.log"
    fi
    if podman inspect "$RUNNER" >/dev/null 2>&1; then
        podman logs "$RUNNER" >"${OUT}/runner.log" 2>&1 || true
        if ! podman cp "${RUNNER}:/results/." "${OUT}/" >/dev/null 2>&1; then
            err "warning: runner produced no /results to collect"
        fi
    fi
    # stdout of this script carries only go test JSON lines, emitted once
    # the events file is complete.
    if [ -f "${OUT}/test-events.jsonl" ]; then
        cat "${OUT}/test-events.jsonl" || true
    fi
}

check_result() {
    local go_exit="" checker_exit=""
    [ -f "${OUT}/go-exit-code" ] && go_exit="$(tr -dc '0-9' <"${OUT}/go-exit-code")"
    [ -f "${OUT}/checker-exit-code" ] && checker_exit="$(tr -dc '0-9' <"${OUT}/checker-exit-code")"
    write_run_info "go_exit=${go_exit:-unavailable}"
    write_run_info "checker_exit=${checker_exit:-unavailable}"
    if [ -f "${OUT}/result.json" ]; then
        grep -q '"status": "pass"' "${OUT}/result.json" \
            && write_run_info "result_status=pass" \
            || write_run_info "result_status=fail"
    else
        write_run_info "result_status=missing"
    fi
    if [ -z "$go_exit" ] || [ -z "$checker_exit" ]; then
        EXIT_SOURCE="result-check"
        FINAL_EXIT=1
        err "result check failed: incomplete results (no go or checker exit code)"
    elif [ "$go_exit" != "0" ]; then
        EXIT_SOURCE="go-test"
        FINAL_EXIT="$go_exit"
        err "go test failed with exit ${go_exit} (details in evidence dir)"
    elif [ "$RUNNER_EXIT" != "0" ]; then
        EXIT_SOURCE="runner"
        FINAL_EXIT=1
        err "infrastructure failure: runner exit ${RUNNER_EXIT} despite go exit 0"
    elif [ "$checker_exit" != "0" ] || ! grep -q '"status": "pass"' "${OUT}/result.json" 2>/dev/null; then
        EXIT_SOURCE="result-check"
        FINAL_EXIT=1
        err "result check failed: verifier rejected the run (see result.json)"
    else
        EXIT_SOURCE="tests"
        FINAL_EXIT=0
    fi
}

cleanup_resources() {
    local rc=0 id
    : >"${OUT}/cleanup.log"
    resource_label() {
        local kind="$1" name="$2"
        case "$kind" in
            container) podman inspect -f '{{ index .Config.Labels "git-repo-backup-it-run" }}' "$name" 2>/dev/null || true ;;
            pod) podman pod inspect -f '{{ index .Labels "git-repo-backup-it-run" }}' "$name" 2>/dev/null || true ;;
            volume) podman volume inspect -f '{{ index .Labels "git-repo-backup-it-run" }}' "$name" 2>/dev/null || true ;;
            image) podman image inspect -f '{{ index .Config.Labels "git-repo-backup-it-run" }}' "$name" 2>/dev/null || true ;;
        esac
    }
    remove_container() {
        local name="$1"
        if podman inspect "$name" >/dev/null 2>&1; then
            if [ "$(resource_label container "$name")" != "$LABEL" ]; then
                err "cleanup refused unowned container: $name"
                rc=1
            elif ! podman rm -f -t 5 "$name" >>"${OUT}/cleanup.log" 2>&1; then
                rc=1
            fi
        fi
    }
    for id in "$RUNNER" "$MINIO_NAME"; do
        remove_container "$id"
    done
    if podman pod inspect "$POD" >/dev/null 2>&1; then
        if [ "$(resource_label pod "$POD")" != "$LABEL" ]; then
            err "cleanup refused unowned pod: $POD"
            rc=1
        elif ! podman pod rm -f "$POD" >>"${OUT}/cleanup.log" 2>&1; then
            rc=1
        fi
    fi
    local vol
    for vol in "${CREATED_VOLUMES[@]}"; do
        if podman volume inspect "$vol" >/dev/null 2>&1; then
            if [ "$(resource_label volume "$vol")" != "$LABEL" ]; then
                err "cleanup refused unowned volume: $vol"
                rc=1
            elif ! podman volume rm "$vol" >>"${OUT}/cleanup.log" 2>&1; then
                rc=1
            fi
        fi
    done
    # Safety net for partially created resources with this run's label.
    for id in $(podman ps -aq --filter "label=${LABEL}" 2>/dev/null); do
        podman rm -f -t 5 "$id" >>"${OUT}/cleanup.log" 2>&1 || rc=1
    done
    for id in $(podman pod ls -q --filter "label=${LABEL}" 2>/dev/null); do
        podman pod rm -f "$id" >>"${OUT}/cleanup.log" 2>&1 || rc=1
    done
    for id in $(podman volume ls -q --filter "label=${LABEL}" 2>/dev/null); do
        podman volume rm "$id" >>"${OUT}/cleanup.log" 2>&1 || rc=1
    done
    if [ "$IMAGE_PREEXISTING" -eq 0 ] && podman image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
        if [ "$(resource_label image "$IMAGE_TAG")" != "$LABEL" ]; then
            err "cleanup refused unowned image tag: $IMAGE_TAG"
            rc=1
        elif ! podman image rm "$IMAGE_TAG" >>"${OUT}/cleanup.log" 2>&1; then
            rc=1
        fi
    fi
    if [ "$rc" -ne 0 ]; then
        CLEANUP_FAILED=1
        err "cleanup reported failures (see cleanup.log)"
    fi
}

on_signal() {
    trap - INT TERM
    SIGNALLED=143
    [ "$1" = "INT" ] && SIGNALLED=130
    err "received SIG$1: stopping runner, collecting evidence, cleaning up"
    if podman inspect "$RUNNER" >/dev/null 2>&1; then
        podman stop -t 10 "$RUNNER" >/dev/null 2>&1 || true
    fi
    wait_runner
    collect_evidence
    write_run_info "exit_source=signal"
    write_run_info "signal=${1}"
    cleanup_resources
    exit "$SIGNALLED"
}

main() {
    parse_args "$@"
    preflight
    create_evidence
    trap 'on_signal INT' INT
    trap 'on_signal TERM' TERM
    build_image
    create_pod
    if [ "$MODE" = "all" ]; then
        prepare_fixture
    fi
    start_runner
    info "runner started; waiting for completion"
    wait_runner
    collect_evidence
    check_result
    cleanup_resources
    write_run_info "exit_source=${EXIT_SOURCE}"
    write_run_info "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [ "$CLEANUP_FAILED" -ne 0 ]; then
        exit 1
    fi
    exit "$FINAL_EXIT"
}

main "$@"
