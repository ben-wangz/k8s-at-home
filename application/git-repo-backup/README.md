# git-repo-backup

Periodic, provider-independent backups of Git repositories over the
standard Git SSH protocol. Each run produces one backup directory (local
PVC) or one object prefix (S3-compatible storage) containing one `tar.gz`
mirror archive per repository, a manifest, a checksum file, and a
`_SUCCESS` commit marker written last.

Works with any SSH Git server: Gitea, GitLab, GitHub, Forgejo, or a plain
`ssh` + `git-shell` host. The repository list is explicit configuration;
there is no provider API integration.

## What is and is not backed up

Included: all objects reachable from the refs the server advertises —
branches, tags (including annotated tags), notes, and other advertised
refs — plus the symbolic `HEAD` and the configuration needed to keep a
bare mirror.

Not included:

- Git LFS content (pointer files only; LFS needs its own backup mechanism).
- Submodule repositories unless every submodule is also a configured entry.
- Provider data: issues, pull requests, permissions, webhooks, branch
  protection, release attachments, CI artifacts, and provider-specific
  hidden refs that are not advertised.
- Any cross-repository transactional snapshot: each repository records one
  successful fetch; the source may change during the run.

## How it works

```
git clone --mirror / git remote update  →  git fsck --full  →  tar.gz + SHA-256
→ manifest.json + checksums.sha256  →  _SUCCESS last  →  retention
```

- One run backs up every configured repository serially; the first failure
  fails the whole run and no `_SUCCESS` is written (all-or-nothing per
  backup directory).
- Local backend: archives are staged in `<mountPath>/.staging/<id>-<owner>`
  and published with an atomic no-replace rename to
  `<mountPath>/backups/<backup-id>`.
- S3 backend: every object is written conditionally; the backup ID is
  reserved by a small permanent claim object under
  `<prefix>/.control/claims/` so concurrent runs can never overwrite each
  other. `_SUCCESS` is the last object uploaded.
- Backups are immutable once published; retention only deletes complete
  backups (`maxBackups`, `maxAge`) and cleans stale incomplete data
  (`incompleteMaxAge`).

## Prerequisites

### SSH key Secret

The chart never generates or stores private keys in Helm state. Create a
read-only identity (a Deploy Key per repository, or a dedicated backup
account for services that do not allow Deploy Key reuse) and create the
Secret from local files:

```bash
kubectl create secret generic git-repo-backup-ssh \
  --from-file=ssh-privatekey=./backup-key
```

Encrypted (passphrase-protected) keys are not supported: the job must
never block on a prompt.

### Pinned known_hosts

The backup always runs with `StrictHostKeyChecking=yes` against a pinned
key file; `ssh-keyscan` is never run automatically, because that would
trust whichever server answers at backup time. Verify the host key through
a trusted channel (provider docs, `ssh-keyscan` from a machine you trust
while you compare the fingerprint out-of-band), then create the ConfigMap:

```bash
ssh-keyscan -p 22 git.example.com   # compare against the fingerprint you obtained
kubectl create configmap git-repo-backup-known-hosts \
  --from-file=known_hosts=./known_hosts
```

A Secret works equally well (`ssh.knownHosts.existingSecret`).

### Supported URL syntax (v1 narrowing)

- `ssh://[user@]host[:port]/path`
- SCP style `[user@]host:path` (no port; bracket IPv6 like `git@[2001:db8::1]:path`)

Repository paths accept ASCII letters, digits, `/ . _ - ~`, and Unicode
letters/digits. Shell metacharacters, spaces, backslashes, passwords,
query strings, and fragments are rejected. Non-standard paths should use
the `ssh://` form.

## Installation

See `chart/examples/` for complete values. Bare `helm lint` fails by
design — the defaults contain no example repositories or credentials.

### 1. Local storage, chart-managed PVC

```bash
helm upgrade --install git-repo-backup ./application/git-repo-backup/chart \
  --namespace backup \
  -f ./application/git-repo-backup/chart/examples/local.yaml
```

The created PVC carries `helm.sh/resource-policy: keep`; uninstalling the
release does not delete backup data.

### 2. Local storage, existing PVC

Use `examples/local-existing-pvc.yaml`. The volume root must be writable
by the pod UID (default `10001`); on root-squash storage an administrator
must pre-create it with the right ownership — see
[Troubleshooting](#troubleshooting).

### 3. S3-compatible storage with static credentials

Use `examples/s3-secret.yaml`:

```bash
kubectl create secret generic git-repo-backup-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

Static credentials are only accepted from a Secret; plaintext values in
Helm are rejected by the schema.

### 4. AWS S3 with workload identity (IRSA)

Use `examples/s3-workload-identity.yaml`. The chart creates a dedicated
ServiceAccount annotated with the IAM role ARN and projects an
`sts.amazonaws.com` audience token; no static credentials exist. The
role's trust policy must allow the cluster OIDC provider and the
serviceAccount subject.

### External repository list

For large or externally managed lists, create a ConfigMap with exactly
this structure (`examples/repositories.yaml`):

```yaml
repositories:
  - name: example-main
    url: git@git.example.com:team/main.git
```

`repositories` (inline) and `repositoriesConfigMap.existingConfigMap` are
mutually exclusive. Changing the list affects future runs only; published
backups stay immutable.

## Manual runs and concurrency

```bash
kubectl create job --from=cronjob/git-repo-backup-git-repo-backup \
  git-repo-backup-manual-20260915
```

`concurrencyPolicy: Forbid` only stops the scheduler from overlapping two
scheduled runs. A manual run can still overlap: the local backend then
fails fast on the volume lock, and the S3 backend fails if the same-second
backup ID is already claimed. Different-second concurrent runs proceed
independently; with a shared cache PVC they serialize on the cache lock or
fail while the other holds it.

## Storage notes

- **Local**: the workspace and (optional) mirror cache are working state,
  not backups. A PVC is not an off-cluster copy; choose the failure domain
  deliberately. Capacity is checked before each run (`backup.minFreeBytes`)
  and ENOSPC during a run aborts it without a marker.
- **S3**: requires conditional writes (`If-None-Match` PutObject and
  conditional CompleteMultipartUpload). AWS S3 and recent MinIO releases
  qualify; services without conditional writes fail loudly instead of
  overwriting. Required permissions on the bucket/prefix:
  `ListBucket`, `GetObject`, `PutObject`, `DeleteObject`,
  `AbortMultipartUpload`, `ListBucketMultipartUploads`,
  `ListMultipartUploadParts`, plus Web Identity (IRSA) and KMS permissions
  when used.
- **S3 claims**: each run reserves its ID with a small permanent JSON
  object under `<prefix>/.control/claims/`. Claims are tombstones, not
  backup data: they are never deleted, do not count toward `maxBackups`,
  and accumulate one small object per run (about 365/year for a daily
  schedule). Other upload tools must not write into the configured prefix.
- **Server-side encryption**: none, `AES256`, or `aws:kms` (which requires
  `kmsKeyId`). TLS verification is always on; an optional custom CA
  (`customCA.*`) is appended to the system trust pool, never a replacement.

## Restore

Select a backup containing `_SUCCESS`, verify, extract, check, and push.
The destination must be an **empty** repository: `git push --mirror`
deletes destination refs that are absent from the backup.

```bash
# local backend
cd /backups/20260914T020000Z
sha256sum --check checksums.sha256
tar -tzf repositories/example-main.tar.gz          # expect one top dir example-main.git/
tar -xzf repositories/example-main.tar.gz --no-same-owner
git -C example-main.git fsck --full
git -C example-main.git remote set-url origin git@replacement.example.com:team/main.git
git -C example-main.git push --mirror origin
```

For S3, first download the whole run prefix (`manifest.json`,
`checksums.sha256`, `repositories/*`) with any S3 client, then run the
same steps. Restoring a single archive still means verifying the manifest
digest and the corresponding checksum line — never trust a half-copied
directory.

Notes:

- The archive's Git config carries the non-sensitive placeholder
  `ssh://git@restore.invalid/repository.git` as its origin URL; the real
  source address is not stored. Pin the replacement host's key before
  restoring.
- `push --mirror` does not change the destination service's default
  branch. After restoring, set the remote symbolic HEAD to the archived
  HEAD (`git symbolic-ref HEAD refs/heads/<branch>` on a plain bare
  server, or the provider's settings UI) and verify refs and OIDs.
- Verify a restore by comparing `git ls-remote` output of the destination
  with the archived refs before decommissioning the source.

## Troubleshooting

| Symptom | Meaning / action |
| --- | --- |
| `prepare` initContainer fails with "volume root is not empty and not owned by the target uid" | The PVC root has wrong ownership. On root-squash storage, pre-create the root directory with UID/GID from `podSecurityContext` (default 10001/10001). |
| Cluster forbids root initContainers | The default design needs one root initContainer with only `CAP_CHOWN` to install the `0400` key for the non-root main container. Clusters forbidding it must pre-provision readable key material and are outside v1's default install; the main container never runs as root. |
| `host_key_verification_failed` | known_hosts does not pin the current server key. Fix the ConfigMap/Secret; never disable host key checking. |
| `authentication_failed` | Key rejected: check Deploy Key / account read access and that the key is unencrypted. |
| `publish_conflict: backup id already exists` | Same-second collision or a concurrent writer; the existing backup was not touched. Re-run. |
| `filesystem_capability_missing` | Volume lacks atomic no-replace rename (e.g. some NFS configurations). Use a filesystem that supports it. |
| Job failed but `published=true` | The backup is complete; retention failed afterwards. The storage-growth problem is surfaced without invalidating the backup. |
| `publication_unknown` | The commit marker response could not be confirmed (e.g. network loss). Re-check for `_SUCCESS` before assuming failure; the run never deletes possibly-committed data. |
| PVC fills up | Check `backup.minFreeBytes` and retention values; workspace/cache are re-creatable working state. |

## Versioning and development

Image and chart versions are managed with `forgekit` (see the repository
root README). The binary's CLI is:

```
git-repo-backup run      --config /etc/git-repo-backup/config/config.yaml
git-repo-backup validate --config /etc/git-repo-backup/config/config.yaml
git-repo-backup version
git-repo-backup prepare  --config /etc/git-repo-backup/config/config.yaml
```

Exit codes: `0` success, `2` configuration/input error, `1` runtime,
publication, or retention error, `130` SIGINT, `143` SIGTERM.

Integration acceptance runs in the existing Kubernetes `develop` namespace;
the host does not need root, Go, sshd, or a container runtime. Build the test
image from `container/test/Containerfile` through the cluster's approved
Kubernetes-native image pipeline and use its immutable digest. The canonical
Job commands are documented in
`build/git-repo-backup.validation.md` and use the image's fixed entry point:

```yaml
command: ["/opt/git-repo-backup-test/in-container.sh"]
args: ["run-tests", "local"] # use all after the TLS MinIO precheck
```

The Job runs with container-internal UID 0 in an isolated Pod, never with a
host mount. `local` runs only `TestLocal*`; `all` also uses the run's TLS
MinIO Service and CA Secret and runs the complete integration package. The
entry point verifies that every discovered test completes with zero skips or
failures and writes Go events, logs, exit codes, and `result.json` under
`/results`. Copy that directory before deleting the Job, then clean only
resources carrying the run label. The existing
`container/test/run-integration.sh` remains a local convenience wrapper; its
result is not Kubernetes acceptance evidence.
