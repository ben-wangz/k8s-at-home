# Usage

Use the backend's client endpoint for all Registry operations. The examples below assume a reachable HTTPS endpoint and installed Docker, Podman, Helm, or ORAS clients.

```sh
HUB=registry.example.com
```

For a trusted local HTTP endpoint, Podman uses `--tls-verify=false`; Helm and ORAS use `--plain-http`. Docker requires an insecure-registry entry in its daemon configuration. Public access should use HTTPS. When client authentication is enabled, log in with credentials from the client Secret; see [Security](security.md).

## Images

Publish an image that already exists locally, using a new tag:

```sh
LOCAL_IMAGE=my-image:build-123
docker tag "$LOCAL_IMAGE" "$HUB/localimage:build-123"
docker push "$HUB/localimage:build-123"
docker pull "$HUB/localimage:build-123"
```

The equivalent Podman commands are:

```sh
podman tag "$LOCAL_IMAGE" "$HUB/localimage:build-123"
podman push "$HUB/localimage:build-123"
podman pull "$HUB/localimage:build-123"
```

`localimage` has no slash and is always local. For a repository containing a slash, its first component is the upstream authority when local content is missing. For example, `team/app` means authority `team`, repository `app`; it is not a special local namespace.

## Mirror pulls

Prefix the original image reference with the Hub endpoint:

```sh
docker pull "$HUB/docker.io/library/busybox:1.36"
```

Registry Hub checks local content first. It accesses the upstream only after a confirmed local miss. `docker.io` uses the Docker Hub transport profile; other authorities use their own HTTPS endpoint. Private upstream images require separately configured upstream credentials.

A successful pull can precede completion of background solidification. A platform manifest can be published by digest once that platform's closure is verified; the requested root tag waits for all referenced platforms and the full original closure. The root task reaches `solidified` after the root reference is published and verified. Check progress through the [task operations](operations.md#solidification-tasks).

Local content remains authoritative after solidification. Later upstream tag changes do not automatically refresh it. Pushes and deletes always affect local content, including repositories with an upstream prefix.

## Helm OCI charts

Use an existing chart directory and a new chart version:

```sh
helm package ./my-chart --destination ./packages
helm push ./packages/my-chart-0.1.0.tgz "oci://$HUB"
helm pull "oci://$HUB/my-chart" --version 0.1.0
```

The package filename, chart name, and version must match its `Chart.yaml`. OCI uploads preserve chart and provenance content.

## ORAS artifacts and Referrers

Run these commands in a working directory containing the files you want to publish:

```sh
oras push --artifact-type application/vnd.example.file.v1 \
  "$HUB/files:build-123" artifact.txt:text/plain
oras pull --output ./downloaded "$HUB/files:build-123"

oras attach --artifact-type application/vnd.example.signature.v1 \
  "$HUB/files:build-123" signature.json:application/json
oras discover --format json "$HUB/files:build-123"
```

Referrers use native support when available and an OCI fallback index otherwise. Attaching a referrer does not verify a signature or interpret its payload. Registry Hub preserves valid OCI artifact bytes and media types.

## Tags and deletion

An existing tag cannot be replaced with a different digest: the first verified publication wins, and a conflicting publication returns `409`. Repeating the same digest is idempotent. Use a new tag for new content.

To delete a manifest, use its digest from a push result or manifest inspection:

```sh
DIGEST=sha256:replace_with_the_manifest_digest
oras manifest delete --force "$HUB/files@$DIGEST"
```

Deletion requires `delete` permission when client authentication is enabled. Protected content can be refused. Deleting a manifest does not immediately free all storage; [GC](operations.md#garbage-collection) reclaims unreferenced Distribution content. Subject deletion does not automatically delete its referrer artifacts.
