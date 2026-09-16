#!/usr/bin/env bash
# Internal fixture library for run-integration.sh: TLS certificate
# generation, in-pod TLS MinIO lifecycle, readiness check, and S3 precheck.
#
# Sourcing this file has no side effects. Executing it directly is refused
# with a migration hint (the standalone start/stop workflow was removed).
# Functions take explicit arguments and never run Podman-independent state;
# all resources must be created by, and cleaned up through, the entry
# script that sources this library.

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    echo "launch-fixtures.sh is an internal library sourced by run-integration.sh." >&2
    echo "Use: $(dirname "${BASH_SOURCE[0]}")/run-integration.sh local|all" >&2
    exit 2
fi

# grb_pod_run LABEL args... -- run a short-lived container, output to stderr.
grb_pod_run() {
    local label="$1"
    shift
    if ! podman run --rm --label "$label" "$@" >&2; then
        return 1
    fi
    return 0
}

# grb_gen_certs IMAGE CA_VOL CERT_VOL -- fixture CA + server certificate.
grb_gen_certs() {
    local image="$1" ca_vol="$2" cert_vol="$3"
    podman run --rm \
        -v "${ca_vol}:/ca-out" \
        -v "${cert_vol}:/certs-out" \
        "$image" /opt/git-repo-backup-test/in-container.sh gen-certs >&2
}

# grb_start_minio IMAGE POD NAME LABEL CERT_VOL DATA_VOL -- detached MinIO
# bound to :9000 inside the pod, serving TLS from the certificate volume.
grb_start_minio() {
    local image="$1" pod="$2" name="$3" label="$4" cert_vol="$5" data_vol="$6"
    podman run -d \
        --pod "$pod" \
        --name "$name" \
        --label "$label" \
        --user 0:0 \
        -v "${cert_vol}:/certs:ro" \
        -v "${data_vol}:/data" \
        -e MINIO_ROOT_USER=gitrepobackup \
        -e MINIO_ROOT_PASSWORD=gitrepobackup-secret \
        "$image" server /data --address :9000 --certs-dir /certs >&2
}

# grb_minio_ready IMAGE POD CA_VOL -- probe the readiness endpoint with the
# fixture CA; total wait bounded at 60 seconds.
grb_minio_ready() {
    local image="$1" pod="$2" ca_vol="$3" waited=0
    while [ "$waited" -lt 60 ]; do
        if podman run --rm --pod "$pod" -v "${ca_vol}:/fixture-ca:ro" \
            "$image" curl --cacert /fixture-ca/ca.crt -fsS \
            --connect-timeout 3 --max-time 10 \
            https://127.0.0.1:9000/minio/health/live >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

# grb_s3_precheck MC_IMAGE POD CA_VOL BUCKET LABEL -- create the bucket and
# prove create/read/delete works with a trusted CA before any test runs.
# The mc image is shell-free, so each step is one explicit invocation that
# receives the alias and CA bundle through the environment; --insecure is
# never used. MC_CA_BUNDLE alone is ignored by current mc releases for
# MC_HOST URLs, so SSL_CERT_FILE carries the same bundle as well.
grb_s3_precheck() {
    local mc_image="$1" pod="$2" ca_vol="$3" bucket="$4" label="$5"
    local -a run_args=(--rm --pod "$pod" --label "$label" --user 0:0
        -v "${ca_vol}:/fixture-ca:ro"
        -e MC_HOST_it="https://gitrepobackup:gitrepobackup-secret@127.0.0.1:9000"
        -e MC_CA_BUNDLE=/fixture-ca/ca.crt
        -e SSL_CERT_FILE=/fixture-ca/ca.crt)
    podman run "${run_args[@]}" "$mc_image" mb --ignore-existing "it/$bucket" >&2 \
        || return 1
    printf 'git-repo-backup-fixture-check' \
        | podman run -i "${run_args[@]}" "$mc_image" pipe "it/$bucket/fixture-check.txt" >&2 \
        || return 1
    local content
    content="$(podman run "${run_args[@]}" "$mc_image" cat "it/$bucket/fixture-check.txt" 2>/dev/null)" \
        || return 1
    if [ "$content" != "git-repo-backup-fixture-check" ]; then
        echo "fixture object round-trip mismatch" >&2
        return 1
    fi
    podman run "${run_args[@]}" "$mc_image" rm "it/$bucket/fixture-check.txt" >&2 \
        || return 1
    if ! content="$(podman run "${run_args[@]}" "$mc_image" ls "it/$bucket/" 2>/dev/null)"; then
        echo "fixture object listing failed after delete" >&2
        return 1
    fi
    if printf '%s\n' "$content" | grep -q fixture-check; then
        echo "fixture object still listed after delete" >&2
        return 1
    fi
    echo "fixture s3 precheck ok" >&2
}

# grb_minio_logs NAME FILE -- dump MinIO container logs for evidence.
grb_minio_logs() {
    local name="$1" file="$2"
    podman logs "$name" >"$file" 2>&1 || true
}
