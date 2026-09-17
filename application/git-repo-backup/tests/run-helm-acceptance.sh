#!/usr/bin/env bash
# Run a black-box acceptance against an installed git-repo-backup Helm chart.
# This script does not build or start a test container.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$APP_DIR/../.." && pwd)"
CHART_DIR="$APP_DIR/chart"

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
INSTALLED=0

usage() {
    cat <<'USAGE'
usage: run-helm-acceptance.sh --mode local|s3 --image IMAGE@sha256:DIGEST \
  --repository-url SSH_URL --ssh-secret NAME \
  (--known-hosts-configmap NAME | --known-hosts-secret NAME) [options]

The script installs the chart in an existing namespace, creates one Job from
the rendered CronJob, checks the production security context, and verifies
one committed backup. It uses only Helm and kubectl.

Options:
  --namespace NAME             Kubernetes namespace (default: develop)
  --release NAME               Helm release name (default: generated)
  --repository-name NAME       Repository entry name (default: example)
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
    [ -n "$SSH_SECRET" ] || usage_die "--ssh-secret is required"
    [ -n "$KNOWN_HOSTS_CONFIGMAP" ] || [ -n "$KNOWN_HOSTS_SECRET" ] \
        || usage_die "one known-hosts source is required"
    [ -z "$KNOWN_HOSTS_CONFIGMAP" ] || [ -z "$KNOWN_HOSTS_SECRET" ] \
        || usage_die "known-hosts ConfigMap and Secret are mutually exclusive"
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
    kubectl -n "$NAMESPACE" get secret "$SSH_SECRET" >/dev/null \
        || die "SSH Secret does not exist: $SSH_SECRET"
    if [ -n "$KNOWN_HOSTS_CONFIGMAP" ]; then
        kubectl -n "$NAMESPACE" get configmap "$KNOWN_HOSTS_CONFIGMAP" >/dev/null \
            || die "known-hosts ConfigMap does not exist: $KNOWN_HOSTS_CONFIGMAP"
    else
        kubectl -n "$NAMESPACE" get secret "$KNOWN_HOSTS_SECRET" >/dev/null \
            || die "known-hosts Secret does not exist: $KNOWN_HOSTS_SECRET"
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
    {
        printf 'run_id=%s\nnamespace=%s\nrelease=%s\nmode=%s\n' \
            "$RUN_ID" "$NAMESPACE" "$RELEASE" "$MODE"
        printf 'image=%s\nrepository_name=%s\nrepository_url=%s\n' \
            "$IMAGE_REF" "$REPOSITORY_NAME" "$REPOSITORY_URL"
        printf 'started_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null | sed 's/^/revision=/' \
            || printf 'revision=unavailable\n'
        git -C "$REPO_ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ' \
            | sed 's/^/dirty_entries=/'
    } >"$EVIDENCE/run-info.txt"
}

write_values() {
    local known_hosts_block
    if [ -n "$KNOWN_HOSTS_CONFIGMAP" ]; then
        known_hosts_block="    existingConfigMap: $(yaml_quote "$KNOWN_HOSTS_CONFIGMAP")"
    else
        known_hosts_block="    existingSecret: $(yaml_quote "$KNOWN_HOSTS_SECRET")"
    fi
    {
        printf 'schedule: %s\nsuspend: true\nbackoffLimit: 0\n' "$(yaml_quote '0 0 1 1 *')"
        printf 'activeDeadlineSeconds: 1800\nsuccessfulJobsHistoryLimit: 0\nfailedJobsHistoryLimit: 0\n'
        printf 'image:\n'
        if [ -n "$IMAGE_REGISTRY" ]; then
            printf '  registry: %s\n' "$(yaml_quote "$IMAGE_REGISTRY")"
        fi
        printf '  repository: %s\n  tag: %s\n  digest: %s\n  pullPolicy: Always\n' \
            "$(yaml_quote "$IMAGE_REPOSITORY")" "$(yaml_quote '0.0.0')" \
            "$(yaml_quote "$IMAGE_DIGEST")"
        printf 'repositories:\n  - name: %s\n    url: %s\n' \
            "$(yaml_quote "$REPOSITORY_NAME")" "$(yaml_quote "$REPOSITORY_URL")"
        printf 'ssh:\n  existingSecret: %s\n  privateKeyKey: ssh-privatekey\n  knownHosts:\n' \
            "$(yaml_quote "$SSH_SECRET")"
        printf '%s\n    key: known_hosts\n' "$known_hosts_block"
        printf 'retention:\n  enabled: false\n'
        if [ "$MODE" = local ]; then
            printf 'storage:\n  type: local\n  local:\n    mountPath: /backup\n    existingClaim: %s\n    persistence:\n      enabled: true\n      size: 1Gi\n' "$(yaml_quote '')"
        else
            printf 'storage:\n  type: s3\n  s3:\n    endpoint: %s\n    region: us-east-1\n    bucket: %s\n    prefix: %s\n    forcePathStyle: true\n    credentialsMode: secret\n    existingSecret: %s\n    accessKeyIdKey: AWS_ACCESS_KEY_ID\n    secretAccessKeyKey: AWS_SECRET_ACCESS_KEY\n' \
                "$(yaml_quote "$S3_ENDPOINT")" "$(yaml_quote "$S3_BUCKET")" \
                "$(yaml_quote "$S3_PREFIX")" "$(yaml_quote "$S3_SECRET")"
            printf 'customCA:\n  existingSecret: %s\n  key: ca.crt\n' "$(yaml_quote "$CA_SECRET")"
        fi
    } >"$EVIDENCE/values.yaml"
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
    python3 - "$EVIDENCE/cronjob.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    cron = json.load(fh)
pod = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]
security = pod.get("securityContext", {})
if security.get("runAsNonRoot") is not True:
    raise SystemExit("pod runAsNonRoot must be true")
if security.get("runAsUser") != 10001 or security.get("runAsGroup") != 10001:
    raise SystemExit("pod must run as uid/gid 10001")
init = next(c for c in pod["initContainers"] if c["name"] == "prepare")
main = next(c for c in pod["containers"] if c["name"] == "git-repo-backup")
if init.get("securityContext", {}).get("runAsUser") != 0:
    raise SystemExit("prepare initContainer must run as uid 0")
main_sec = main.get("securityContext", {})
if main_sec.get("readOnlyRootFilesystem") is not True:
    raise SystemExit("main container must use a read-only root filesystem")
if main_sec.get("allowPrivilegeEscalation") is not False:
    raise SystemExit("main container must forbid privilege escalation")
if "ALL" not in main_sec.get("capabilities", {}).get("drop", []):
    raise SystemExit("main container must drop ALL capabilities")
PY
}

run_chart_job() {
    JOB="$RELEASE-manual-$RUN_ID"
    kubectl -n "$NAMESPACE" create job "$JOB" --from="cronjob/$CRONJOB" \
        >"$EVIDENCE/create-job.log"
    kubectl -n "$NAMESPACE" label job "$JOB" \
        "git-repo-backup.validation/run=$RUN_ID" --overwrite >/dev/null
    if ! kubectl -n "$NAMESPACE" wait --for=condition=complete \
        --timeout=35m "job/$JOB" >"$EVIDENCE/job-wait.log" 2>&1; then
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
    python3 - "$EVIDENCE/job.yaml" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    job = json.load(fh)
if job.get("status", {}).get("succeeded") != 1:
    raise SystemExit("chart-created Job did not report one successful completion")
PY
}

create_local_checker() {
    PVC="$(kubectl -n "$NAMESPACE" get pvc -l "app.kubernetes.io/instance=$RELEASE" \
        -o jsonpath='{.items[0].metadata.name}')"
    [ -n "$PVC" ] || die "chart did not create a local backup PVC"
    local checker
    checker="$RELEASE-inspect"
    cat >"$EVIDENCE/local-checker.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $checker
  namespace: $NAMESPACE
  labels:
    git-repo-backup.validation/run: $RUN_ID
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        git-repo-backup.validation/run: $RUN_ID
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      containers:
        - name: inspect
          image: $INSPECTOR_IMAGE
          command: ["sh", "-ec"]
          args:
            - |
              count=0
              for run in /backup/backups/*; do
                [ -d "\$run" ] || continue
                [ -f "\$run/_SUCCESS" ] || continue
                [ -f "\$run/manifest.json" ] || exit 1
                [ -f "\$run/checksums.sha256" ] || exit 1
                find "\$run/repositories" -type f -name '*.tar.gz' -print -quit | grep -q .
                count=\$((count + 1))
              done
              [ "\$count" -eq 1 ]
          volumeMounts:
            - name: backup
              mountPath: /backup
              readOnly: true
      volumes:
        - name: backup
          persistentVolumeClaim:
            claimName: $PVC
EOF
    kubectl apply -f "$EVIDENCE/local-checker.yaml" >/dev/null
    kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=5m "job/$checker" \
        >"$EVIDENCE/local-checker-wait.log"
    kubectl -n "$NAMESPACE" logs "job/$checker" >"$EVIDENCE/local-checker.log" 2>&1 || true
    kubectl -n "$NAMESPACE" delete job "$checker" --ignore-not-found >/dev/null
}

create_s3_checker() {
    local checker
    checker="$RELEASE-inspect"
    cat >"$EVIDENCE/s3-checker.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $checker
  namespace: $NAMESPACE
  labels:
    git-repo-backup.validation/run: $RUN_ID
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        git-repo-backup.validation/run: $RUN_ID
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      containers:
        - name: inspect
          image: $MC_IMAGE
          env:
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: $S3_SECRET
                  key: AWS_ACCESS_KEY_ID
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: $S3_SECRET
                  key: AWS_SECRET_ACCESS_KEY
            - name: SSL_CERT_FILE
              value: /fixture-ca/ca.crt
          command: ["/bin/sh", "-ec"]
          args:
            - |
              export MC_CONFIG_DIR=/tmp/mc
              mkdir -p /tmp/mc/certs/CAs
              cp /fixture-ca/ca.crt /tmp/mc/certs/CAs/ca.crt
              mc alias set backup "$S3_ENDPOINT" "\$AWS_ACCESS_KEY_ID" "\$AWS_SECRET_ACCESS_KEY" --api S3v4 --path auto
              mc ls --recursive "backup/$S3_BUCKET/$S3_PREFIX/" | grep -E '[[:space:]]_SUCCESS$'
          volumeMounts:
            - name: ca
              mountPath: /fixture-ca
              readOnly: true
      volumes:
        - name: ca
          secret:
            secretName: $CA_SECRET
            items:
              - key: ca.crt
                path: ca.crt
EOF
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
