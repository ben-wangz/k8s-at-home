# Helm acceptance tests

This directory contains the black-box acceptance entry point for
git-repo-backup. The test installs the chart in an existing Kubernetes
namespace, creates a one-shot Job from the installed CronJob, and verifies
the published backup. It does not build a test image and does not run the
application directly from a source or test container.

The source repository and, for SSH URLs, its SSH private-key Secret are
supplied by the caller. The default SSH policy is `accept-new`: a known-hosts
ConfigMap/Secret may be supplied as an optional seed, but it is not required.
An `https://` URL automatically selects the SSH-disabled chart mode, so
public HTTPS repositories can be tested without an SSH Secret or known-hosts
resource. For SSH runs, the script also mounts the chart-created state PVC
after the backup Job and requires `state/known_hosts` to be non-empty, proving
that the first-connection key was persisted. It does not rotate the fixture
host key, so changed-key rejection remains a separate negative test. The
repository must be disposable; the script never changes the source repository.

The production image must be a fixed digest that the cluster can pull. The
local checker also needs a fixed digest image containing sh, find, and the
usual BusyBox file tools. S3 mode needs a fixed digest minio/mc image, an S3
Secret with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and a Secret
containing the endpoint CA as ca.crt.

Example local acceptance:

    application/git-repo-backup/tests/run-helm-acceptance.sh \
      --mode local \
      --image registry.example/git-repo-backup@sha256:<production-digest> \
      --repository-name fixture \
      --repository-url ssh://git@fixture-git.develop.svc:2222/fixture.git \
      --ssh-secret git-repo-backup-it-ssh \
      --known-hosts-configmap git-repo-backup-it-known-hosts \
      --inspector-image registry.example/busybox@sha256:<busybox-digest>

Example S3 acceptance:

    application/git-repo-backup/tests/run-helm-acceptance.sh \
      --mode s3 \
      --image registry.example/git-repo-backup@sha256:<production-digest> \
      --repository-url ssh://git@fixture-git.develop.svc:2222/fixture.git \
      --ssh-secret git-repo-backup-it-ssh \
      --known-hosts-configmap git-repo-backup-it-known-hosts \
      --s3-endpoint https://minio.develop.svc:9000 \
      --s3-bucket git-repo-backup-it \
      --s3-prefix helm-acceptance \
      --s3-secret git-repo-backup-it-s3 \
      --ca-secret git-repo-backup-it-ca \
      --mc-image registry.example/minio/mc@sha256:<mc-digest>

The default namespace is develop. Use --output-dir to store manifests, Helm
output, Job logs, and checker manifests in a new or empty directory.
Resources created by the script are removed on exit. --keep preserves the
release and generated resources for diagnosis. External SSH, S3, and CA
Secrets are never deleted.

For an isolated HTTP-only S3 fixture, pass `--allow-insecure-http` together
with an `http://` endpoint. The flag is disabled by default and is rejected
for HTTPS endpoints.

Example public HTTPS acceptance (no SSH options are needed):

    application/git-repo-backup/tests/run-helm-acceptance.sh \
      --mode local \
      --image registry.example/git-repo-backup@sha256:<production-digest> \
      --repository-url https://github.com/ben-wangz/k8s-at-home.git \
      --inspector-image registry.example/busybox@sha256:<busybox-digest>

The Go tests under src/integration remain opt-in developer tests. They are
not the Helm acceptance path and no longer have a container runner under
container/test. The acceptance script is intentionally only the workflow
orchestrator: `manifests/` contains the local, S3, known-hosts, and values
templates; `render-template.py` substitutes checked runtime values; and
`check-cronjob.py` owns the rendered security-context assertions.
