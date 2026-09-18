#!/usr/bin/env bash
# Run a black-box acceptance against an installed git-repo-backup Helm chart.
# This script does not build or start a test container.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$APP_DIR/../.." && pwd)"
CHART_DIR="$APP_DIR/chart"
TEMPLATE_DIR="$SCRIPT_DIR/manifests"

render_template() {
    local template="$1"
    local output="$2"
    shift 2
    python3 "$SCRIPT_DIR/render-template.py" "$template" "$output" "$@"
}

env_or() {
    local value
    value="$(printenv "$1" 2>/dev/null || true)"
    if [ -n "$value" ]; then
        printf '%s' "$value"
    else
        printf '%s' "$2"
    fi
}

MODE=""
SSH_ENABLED=1
NAMESPACE="$(env_or GIT_REPO_BACKUP_TEST_NAMESPACE develop)"
RELEASE=""
IMAGE_REF="$(env_or GIT_REPO_BACKUP_TEST_IMAGE "")"
REPOSITORY_NAME="example"
REPOSITORY_URL="$(env_or GIT_REPO_BACKUP_TEST_REPOSITORY_URL "")"
SSH_SECRET="$(env_or GIT_REPO_BACKUP_TEST_SSH_SECRET "")"
KNOWN_HOSTS_CONFIGMAP="$(env_or GIT_REPO_BACKUP_TEST_KNOWN_HOSTS_CONFIGMAP "")"
KNOWN_HOSTS_SECRET="$(env_or GIT_REPO_BACKUP_TEST_KNOWN_HOSTS_SECRET "")"
S3_ENDPOINT="$(env_or GIT_REPO_BACKUP_TEST_S3_ENDPOINT "")"
S3_BUCKET="$(env_or GIT_REPO_BACKUP_TEST_S3_BUCKET "")"
S3_PREFIX="$(env_or GIT_REPO_BACKUP_TEST_S3_PREFIX helm-acceptance)"
S3_SECRET="$(env_or GIT_REPO_BACKUP_TEST_S3_SECRET "")"
CA_SECRET="$(env_or GIT_REPO_BACKUP_TEST_CA_SECRET "")"
INSPECTOR_IMAGE="$(env_or GIT_REPO_BACKUP_TEST_INSPECTOR_IMAGE "")"
MC_IMAGE="$(env_or GIT_REPO_BACKUP_TEST_MC_IMAGE "")"
OUTPUT_DIR=""
KEEP_RESOURCES=0
RUN_ID="grb-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
EVIDENCE=""
CRONJOB=""
JOB=""
PVC=""
SSH_STATE_PVC=""
INSTALLED=0

usage() {
    cat <<'USAGE'
usage: run-helm-acceptance.sh --mode local|s3 --image IMAGE@sha256:DIGEST \
  --repository-url SSH_OR_HTTPS_URL [SSH options] [options]

The script installs the chart in an existing namespace, creates one Job from
the rendered CronJob, checks the production security context, and verifies
one committed backup. It uses only Helm and kubectl.

Options:
  --namespace NAME             Kubernetes namespace (default: develop)
  --release NAME               Helm release name (default: generated)
  --repository-name NAME       Repository entry name (default: example)
  --ssh-secret NAME             SSH private-key Secret (required for SSH URLs)
  --known-hosts-configmap NAME known_hosts ConfigMap seed (optional)
  --known-hosts-secret NAME    known_hosts Secret seed (optional)
  --s3-endpoint URL            S3 endpoint for mode s3
  --s3-bucket NAME             S3 bucket for mode s3
  --s3-prefix PREFIX           S3 prefix (default: helm-acceptance)
  --s3-secret NAME             Secret with AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
  --ca-secret NAME              Secret containing ca.crt for S3 TLS
  --inspector-image IMAGE       Fixed digest image used to inspect a local PVC
  --mc-image IMAGE              Fixed digest minio/mc image for S3 verification
  --output-dir ABS_PATH         Evidence directory
  --keep                        Keep the Helm release and generated resources
  --help                        Show this help
USAGE
}

die() {
    printf 'run-helm-acceptance: error: %s\n' "$*" >&2
    exit 1
}

usage_die() {
    printf 'run-helm-acceptance: usage error: %s\n\n' "$*" >&2
    usage >&2
    exit 2
}

yaml_quote() {
    local value
    value="$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"
    printf '"%s"' "$value"
}

valid_digest_image() {
    [[ "$1" =~ ^[a-z0-9./:_-]+@sha256:[0-9a-f]{64}$ ]]
}

parse_image() {
    local name
    valid_digest_image "$IMAGE_REF" || usage_die "--image must be a lowercase immutable digest reference"
    IMAGE_DIGEST="$(printf '%s' "$IMAGE_REF" | awk -F@ '{print $2}')"
    name="$(printf '%s' "$IMAGE_REF" | awk -F@ '{print $1}')"
    if [[ "$name" == */* ]]; then
        IMAGE_REGISTRY="$(printf '%s' "$name" | cut -d/ -f1)"
        IMAGE_REPOSITORY="$(printf '%s' "$name" | cut -d/ -f2-)"
    else
        IMAGE_REGISTRY=""
        IMAGE_REPOSITORY="$name"
    fi
}

parse_args() {
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --help) usage; exit 0 ;;
            --mode|--namespace|--release|--image|--repository-name|--repository-url|\
            --ssh-secret|--known-hosts-configmap|--known-hosts-secret|--s3-endpoint|\
            --s3-bucket|--s3-prefix|--s3-secret|--ca-secret|--inspector-image|\
            --mc-image|--output-dir)
                [ "$#" -ge 2 ] || usage_die "$1 requires a value"
                case "$1" in
                    --mode) MODE="$2" ;;
                    --namespace) NAMESPACE="$2" ;;
                    --release) RELEASE="$2" ;;
                    --image) IMAGE_REF="$2" ;;
                    --repository-name) REPOSITORY_NAME="$2" ;;
                    --repository-url) REPOSITORY_URL="$2" ;;
                    --ssh-secret) SSH_SECRET="$2" ;;
                    --known-hosts-configmap) KNOWN_HOSTS_CONFIGMAP="$2" ;;
                    --known-hosts-secret) KNOWN_HOSTS_SECRET="$2" ;;
                    --s3-endpoint) S3_ENDPOINT="$2" ;;
                    --s3-bucket) S3_BUCKET="$2" ;;
                    --s3-prefix) S3_PREFIX="$2" ;;
                    --s3-secret) S3_SECRET="$2" ;;
                    --ca-secret) CA_SECRET="$2" ;;
                    --inspector-image) INSPECTOR_IMAGE="$2" ;;
                    --mc-image) MC_IMAGE="$2" ;;
                    --output-dir) OUTPUT_DIR="$2" ;;
                esac
                shift 2
                ;;
            --keep) KEEP_RESOURCES=1; shift ;;
            *) usage_die "unknown option: $1" ;;
        esac
    done
    [ "$MODE" = local ] || [ "$MODE" = s3 ] || usage_die "--mode must be local or s3"
    [ -n "$IMAGE_REF" ] || usage_die "--image is required"
    [ -n "$REPOSITORY_URL" ] || usage_die "--repository-url is required"
    if [[ "${REPOSITORY_URL,,}" == https://* ]]; then
        SSH_ENABLED=0
    else
        [ -n "$SSH_SECRET" ] || usage_die "--ssh-secret is required for an SSH URL"
        [ -z "$KNOWN_HOSTS_CONFIGMAP" ] || [ -z "$KNOWN_HOSTS_SECRET" ] \
            || usage_die "known-hosts ConfigMap and Secret are mutually exclusive"
    fi
    if [ "$MODE" = s3 ]; then
        [ -n "$S3_ENDPOINT" ] || usage_die "--s3-endpoint is required for s3"
        [ -n "$S3_BUCKET" ] || usage_die "--s3-bucket is required for s3"
        [ -n "$S3_SECRET" ] || usage_die "--s3-secret is required for s3"
        [ -n "$CA_SECRET" ] || usage_die "--ca-secret is required for s3"
        valid_digest_image "$MC_IMAGE" || usage_die "--mc-image must be a fixed digest reference"
    else
        valid_digest_image "$INSPECTOR_IMAGE" || usage_die "--inspector-image must be a fixed digest reference"
    fi
    [ -n "$RELEASE" ] || RELEASE="$RUN_ID"
    [[ "$RELEASE" =~ ^[a-z0-9]([a-z0-9-]{0,29})$ ]] \
        || usage_die "--release must be a lowercase DNS label of at most 30 characters"
    if [ -n "$OUTPUT_DIR" ]; then
        [[ "$OUTPUT_DIR" = /* ]] || usage_die "--output-dir must be absolute"
        [ ! -e "$OUTPUT_DIR" ] || [ -z "$(find "$OUTPUT_DIR" -mindepth 1 -maxdepth 1 -print -quit)" ] \
            || usage_die "--output-dir must be new or empty"
        EVIDENCE="$OUTPUT_DIR"
    else
        EVIDENCE="$REPO_ROOT/build/git-repo-backup-helm-validation/$RUN_ID"
    fi
}

preflight() {
    command -v helm >/dev/null 2>&1 || die "helm is required"
    command -v kubectl >/dev/null 2>&1 || die "kubectl is required"
    command -v python3 >/dev/null 2>&1 || die "python3 is required for manifest checks"
    kubectl get namespace "$NAMESPACE" >/dev/null || die "namespace does not exist: $NAMESPACE"
    if helm status "$RELEASE" --namespace "$NAMESPACE" >/dev/null 2>&1; then
        die "Helm release already exists: $RELEASE"
    fi
    if [ "$SSH_ENABLED" -eq 1 ]; then
        kubectl -n "$NAMESPACE" get secret "$SSH_SECRET" >/dev/null \
            || die "SSH Secret does not exist: $SSH_SECRET"
        if [ -n "$KNOWN_HOSTS_CONFIGMAP" ]; then
            kubectl -n "$NAMESPACE" get configmap "$KNOWN_HOSTS_CONFIGMAP" >/dev/null \
                || die "known-hosts ConfigMap does not exist: $KNOWN_HOSTS_CONFIGMAP"
        elif [ -n "$KNOWN_HOSTS_SECRET" ]; then
            kubectl -n "$NAMESPACE" get secret "$KNOWN_HOSTS_SECRET" >/dev/null \
                || die "known-hosts Secret does not exist: $KNOWN_HOSTS_SECRET"
        fi
    fi
    if [ "$MODE" = s3 ]; then
        kubectl -n "$NAMESPACE" get secret "$S3_SECRET" >/dev/null \
            || die "S3 Secret does not exist: $S3_SECRET"
        kubectl -n "$NAMESPACE" get secret "$CA_SECRET" >/dev/null \
            || die "CA Secret does not exist: $CA_SECRET"
    fi
}

record_run_info() {
    mkdir -p "$EVIDENCE"
    local repository_scheme=ssh
    if [[ "${REPOSITORY_URL,,}" == https://* ]]; then
        repository_scheme=https
    fi
    {
        printf 'run_id=%s\nnamespace=%s\nrelease=%s\nmode=%s\n' \
            "$RUN_ID" "$NAMESPACE" "$RELEASE" "$MODE"
        printf 'image=%s\nrepository_name=%s\nrepository_scheme=%s\nssh_enabled=%s\n' \
            "$IMAGE_REF" "$REPOSITORY_NAME" "$repository_scheme" "$SSH_ENABLED"
        if [ "$SSH_ENABLED" -eq 1 ]; then
            printf 'host_key_policy=accept-new\n'
        else
            printf 'host_key_policy=disabled\n'
        fi
        printf 'started_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null | sed 's/^/revision=/' \
            || printf 'revision=unavailable\n'
        git -C "$REPO_ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ' \
            | sed 's/^/dirty_entries=/'
    } >"$EVIDENCE/run-info.txt"
}

write_values() {
    local image_registry_line=""
    local ssh_config
    local s3_endpoint_yaml='""' s3_bucket_yaml='""' s3_prefix_yaml='""'
    local s3_secret_yaml='""' ca_secret_yaml='""'

    if [ -n "$IMAGE_REGISTRY" ]; then
        image_registry_line="  registry: $(yaml_quote "$IMAGE_REGISTRY")"
    fi
    if [ "$SSH_ENABLED" -eq 1 ]; then
        ssh_config="$(printf 'ssh:\n  enabled: true\n  hostKeyPolicy: accept-new\n  existingSecret: %s\n  privateKeyKey: ssh-privatekey' "$(yaml_quote "$SSH_SECRET")")"
        if [ -n "$KNOWN_HOSTS_CONFIGMAP" ]; then
            ssh_config="${ssh_config}"$'\n'"$(printf '  knownHosts:\n    existingConfigMap: %s\n    key: known_hosts' "$(yaml_quote "$KNOWN_HOSTS_CONFIGMAP")")"
        elif [ -n "$KNOWN_HOSTS_SECRET" ]; then
            ssh_config="${ssh_config}"$'\n'"$(printf '  knownHosts:\n    existingSecret: %s\n    key: known_hosts' "$(yaml_quote "$KNOWN_HOSTS_SECRET")")"
        fi
    else
        ssh_config=$'ssh:\n  enabled: false'
    fi
    if [ "$MODE" = s3 ]; then
        s3_endpoint_yaml="$(yaml_quote "$S3_ENDPOINT")"
        s3_bucket_yaml="$(yaml_quote "$S3_BUCKET")"
        s3_prefix_yaml="$(yaml_quote "$S3_PREFIX")"
        s3_secret_yaml="$(yaml_quote "$S3_SECRET")"
        ca_secret_yaml="$(yaml_quote "$CA_SECRET")"
    fi

    render_template "$TEMPLATE_DIR/values-$MODE.yaml.tpl" "$EVIDENCE/values.yaml" \
        IMAGE_REGISTRY_LINE "$image_registry_line" \
        IMAGE_REPOSITORY "$(yaml_quote "$IMAGE_REPOSITORY")" \
        IMAGE_DIGEST "$(yaml_quote "$IMAGE_DIGEST")" \
        REPOSITORY_NAME "$(yaml_quote "$REPOSITORY_NAME")" \
        REPOSITORY_URL "$(yaml_quote "$REPOSITORY_URL")" \
        SSH_CONFIG "$ssh_config" \
        S3_ENDPOINT "$s3_endpoint_yaml" \
        S3_BUCKET "$s3_bucket_yaml" \
        S3_PREFIX "$s3_prefix_yaml" \
        S3_SECRET "$s3_secret_yaml" \
        CA_SECRET "$ca_secret_yaml"
}

render_and_install() {
    helm lint "$CHART_DIR" --strict -f "$EVIDENCE/values.yaml" >"$EVIDENCE/helm-lint.log" 2>&1
    helm template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
        -f "$EVIDENCE/values.yaml" >"$EVIDENCE/rendered.yaml"
    kubectl apply --dry-run=server -f "$EVIDENCE/rendered.yaml" >"$EVIDENCE/api-dry-run.log"
    helm upgrade --install "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
        -f "$EVIDENCE/values.yaml" >"$EVIDENCE/helm-install.log"
    INSTALLED=1
    kubectl -n "$NAMESPACE" get cronjob -l "app.kubernetes.io/instance=$RELEASE" \
        -o yaml >"$EVIDENCE/cronjob.yaml"
    CRONJOB="$(kubectl -n "$NAMESPACE" get cronjob \
        -l "app.kubernetes.io/instance=$RELEASE" -o jsonpath='{.items[0].metadata.name}')"
    [ -n "$CRONJOB" ] || die "installed release has no CronJob"
    kubectl -n "$NAMESPACE" get cronjob "$CRONJOB" -o json >"$EVIDENCE/cronjob.json"
    python3 "$SCRIPT_DIR/check-cronjob.py" "$EVIDENCE/cronjob.json" "$SSH_ENABLED"
}

run_chart_job() {
    JOB="$RELEASE-manual-$RUN_ID"
    kubectl -n "$NAMESPACE" create job "$JOB" --from="cronjob/$CRONJOB" \
        >"$EVIDENCE/create-job.log"
    kubectl -n "$NAMESPACE" label job "$JOB" \
        "git-repo-backup.validation/run=$RUN_ID" --overwrite >/dev/null
    if ! wait_for_job; then
        kubectl -n "$NAMESPACE" describe job "$JOB" >"$EVIDENCE/job-describe.log" || true
        kubectl -n "$NAMESPACE" get job "$JOB" -o yaml >"$EVIDENCE/job.yaml" || true
        kubectl -n "$NAMESPACE" get pod -l "job-name=$JOB" -o yaml >"$EVIDENCE/pod.yaml" || true
        kubectl -n "$NAMESPACE" logs "job/$JOB" --all-containers >"$EVIDENCE/job.log" 2>&1 || true
        return 1
    fi
    kubectl -n "$NAMESPACE" get job "$JOB" -o yaml >"$EVIDENCE/job.yaml"
    kubectl -n "$NAMESPACE" get pod -l "job-name=$JOB" -o yaml >"$EVIDENCE/pod.yaml"
    kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp >"$EVIDENCE/events.log" || true
    kubectl -n "$NAMESPACE" logs "job/$JOB" --all-containers >"$EVIDENCE/job.log" 2>&1 || true
    [ "$(kubectl -n "$NAMESPACE" get job "$JOB" -o jsonpath='{.status.succeeded}')" = "1" ] \
        || die "chart-created Job did not report one successful completion"
}

wait_for_job() {
    local deadline=$((SECONDS + 2100))
    : >"$EVIDENCE/job-wait.log"
    while (( SECONDS < deadline )); do
        local conditions
        if ! conditions="$(kubectl -n "$NAMESPACE" get job "$JOB" \
            -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}' \
            2>&1)"; then
            printf '%s\n' "$conditions" >>"$EVIDENCE/job-wait.log"
            return 1
        fi
        printf '%s\n' "$conditions" >>"$EVIDENCE/job-wait.log"
        if grep -q '^Complete=True$' <<<"$conditions"; then
            return 0
        fi
        if grep -q '^Failed=True$' <<<"$conditions"; then
            return 1
        fi
        sleep 5
    done
    printf 'timed out waiting for Job completion\n' >>"$EVIDENCE/job-wait.log"
    return 1
}

check_known_hosts_state() {
    [ "$SSH_ENABLED" -eq 1 ] || return 0
    SSH_STATE_PVC="$(kubectl -n "$NAMESPACE" get pvc \
        -l "app.kubernetes.io/instance=$RELEASE" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
        | awk '/-ssh-known-hosts$/ {print; exit}')"
    [ -n "$SSH_STATE_PVC" ] || die "chart did not create an accept-new known-hosts PVC"

    local checker image
    checker="$RELEASE-known-hosts"
    if [ "$MODE" = local ]; then
        image="$INSPECTOR_IMAGE"
    else
        image="$MC_IMAGE"
    fi
    render_template "$TEMPLATE_DIR/known-hosts-checker.yaml" \
        "$EVIDENCE/known-hosts-checker.yaml" \
        CHECKER_NAME "$(yaml_quote "$checker")" \
        NAMESPACE "$(yaml_quote "$NAMESPACE")" \
        RUN_ID "$(yaml_quote "$RUN_ID")" \
        CHECKER_IMAGE "$(yaml_quote "$image")" \
        SSH_STATE_PVC "$(yaml_quote "$SSH_STATE_PVC")"
    kubectl apply -f "$EVIDENCE/known-hosts-checker.yaml" >/dev/null
    kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=5m "job/$checker" \
        >"$EVIDENCE/known-hosts-checker-wait.log"
    kubectl -n "$NAMESPACE" logs "job/$checker" >"$EVIDENCE/known-hosts-checker.log" 2>&1 || true
    kubectl -n "$NAMESPACE" delete job "$checker" --ignore-not-found >/dev/null
}

create_local_checker() {
    PVC="$(kubectl -n "$NAMESPACE" get pvc -l "app.kubernetes.io/instance=$RELEASE" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | awk '/-backup$/ {print; exit}')"
    [ -n "$PVC" ] || die "chart did not create a local backup PVC"
    local checker
    checker="$RELEASE-inspect"
    render_template "$TEMPLATE_DIR/local-checker.yaml" \
        "$EVIDENCE/local-checker.yaml" \
        CHECKER_NAME "$(yaml_quote "$checker")" \
        NAMESPACE "$(yaml_quote "$NAMESPACE")" \
        RUN_ID "$(yaml_quote "$RUN_ID")" \
        INSPECTOR_IMAGE "$(yaml_quote "$INSPECTOR_IMAGE")" \
        PVC "$(yaml_quote "$PVC")"
    kubectl apply -f "$EVIDENCE/local-checker.yaml" >/dev/null
    kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=5m "job/$checker" \
        >"$EVIDENCE/local-checker-wait.log"
    kubectl -n "$NAMESPACE" logs "job/$checker" >"$EVIDENCE/local-checker.log" 2>&1 || true
    kubectl -n "$NAMESPACE" delete job "$checker" --ignore-not-found >/dev/null
}

create_s3_checker() {
    local checker
    checker="$RELEASE-inspect"
    render_template "$TEMPLATE_DIR/s3-checker.yaml" \
        "$EVIDENCE/s3-checker.yaml" \
        CHECKER_NAME "$(yaml_quote "$checker")" \
        NAMESPACE "$(yaml_quote "$NAMESPACE")" \
        RUN_ID "$(yaml_quote "$RUN_ID")" \
        MC_IMAGE "$(yaml_quote "$MC_IMAGE")" \
        S3_SECRET "$(yaml_quote "$S3_SECRET")" \
        CA_SECRET "$(yaml_quote "$CA_SECRET")" \
        S3_ENDPOINT "$(yaml_quote "$S3_ENDPOINT")" \
        S3_BUCKET "$(yaml_quote "$S3_BUCKET")" \
        S3_PREFIX "$(yaml_quote "$S3_PREFIX")"
    kubectl apply -f "$EVIDENCE/s3-checker.yaml" >/dev/null
    kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=5m "job/$checker" \
        >"$EVIDENCE/s3-checker-wait.log"
    kubectl -n "$NAMESPACE" logs "job/$checker" >"$EVIDENCE/s3-checker.log" 2>&1 || true
    kubectl -n "$NAMESPACE" delete job "$checker" --ignore-not-found >/dev/null
}

cleanup() {
    local status
    status=$?
    if [ "$KEEP_RESOURCES" -eq 1 ]; then
        printf 'resources_kept=true\n' >>"$EVIDENCE/run-info.txt" 2>/dev/null || true
        return "$status"
    fi
    if [ "$status" -ne 0 ]; then
        printf 'status=fail\n' >>"$EVIDENCE/run-info.txt" 2>/dev/null || true
    fi
    kubectl -n "$NAMESPACE" delete job \
        -l "git-repo-backup.validation/run=$RUN_ID" --ignore-not-found >/dev/null 2>&1 || true
    [ -z "$JOB" ] || kubectl -n "$NAMESPACE" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
    if [ "$INSTALLED" -eq 1 ]; then
        helm uninstall "$RELEASE" --namespace "$NAMESPACE" >"$EVIDENCE/helm-uninstall.log" 2>&1 || true
        kubectl -n "$NAMESPACE" delete pvc -l "app.kubernetes.io/instance=$RELEASE" \
            --ignore-not-found >/dev/null 2>&1 || true
    fi
    printf 'resources_kept=false\n' >>"$EVIDENCE/run-info.txt" 2>/dev/null || true
    return "$status"
}

main() {
    parse_args "$@"
    parse_image
    preflight
    record_run_info
    write_values
    trap cleanup EXIT
    render_and_install
    run_chart_job
    check_known_hosts_state
    if [ "$MODE" = local ]; then
        create_local_checker
    else
        create_s3_checker
    fi
    printf 'status=pass\n' >>"$EVIDENCE/run-info.txt"
    printf 'PASS: Helm release %s completed one %s backup; evidence=%s\n' \
        "$RELEASE" "$MODE" "$EVIDENCE"
}

main "$@"
