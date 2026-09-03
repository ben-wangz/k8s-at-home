# codespace

`codespace` is an Ubuntu 24.04 development container with SSH access and a
Podman-in-container runtime.

Run it as a privileged container so the nested Podman runtime can create
namespaces, mounts, and devices.

Preinstalled components include:

- Go 1.26.5, Node.js 24.13.1, and Python 3
- kubectl 1.35.4 and Helm 4.1.4
- Claude Code (`claude`), Codex (`codex`), Grok (`grok` and `grok-agent`), and
  Cursor Agent (`cursor-agent`) development CLIs (under `/usr/local`, not home)
- Playwright 1.61.1 with Chromium; amd64 images also include Google Chrome
- poppler-utils, pdftk, and img2pdf
- Common development, terminal, archive, and network diagnostic tools,
  including `less`
- English and Simplified Chinese UTF-8 locales

Cursor does not currently publish an npm package for its CLI. The image uses
`@nothumanwork/cursor-agent-cli` to install the official Cursor Agent binary via
npm and exposes it only as `cursor-agent`.

The image ships a `coder` user (UID 1000) with passwordless sudo for
unattended coding agents. The configured SSH public key authenticates only
`coder`; use `sudo` for administrative commands.

Tool installs stay on global paths so a volume mounted at `/home/coder` only
holds user state and does not hide image binaries. The chart init container
`prepare-home` mounts the home PVC at `/mnt/home`, seeds missing `.bashrc` /
`.profile` from `/etc/skel`, and hands ownership to `coder` with a one-time
recursive `chown` when the volume owner differs (for example a PVC migrated
from the previous `/root` mount).

## Build

```bash
podman build \
  --file application/codespace/container/Containerfile \
  --tag localhost/k8s-at-home-codespace-base:dev \
  .
```

The default base image is `docker.io/library/ubuntu:24.04`. Override
`UBUNTU_IMAGE` if a mirror is preferred. The Go archive is downloaded from
`https://go.dev/dl`.

## Run

```bash
export SSH_PUBLIC_KEY="$(cat ~/.ssh/id_ed25519.pub)"

podman run --rm --privileged \
  --name codespace \
  --publish 2222:22 \
  --env AUTHORIZED_KEYS="${SSH_PUBLIC_KEY}" \
  --env PODMAN_NETWORK_SUBNET=10.250.0.0/16 \
  --env PODMAN_NETWORK_GATEWAY=10.250.0.1 \
  localhost/k8s-at-home-codespace-base:dev
```

Connect over SSH as `coder`:

```bash
ssh -p 2222 coder@127.0.0.1
```

Then verify the nested runtime:

```bash
sudo podman run --rm docker.io/library/alpine:latest \
  echo "Hello from Podman in codespace"
```

The nested Podman runtime is rootful. Run it with `sudo` from the `coder`
account so container storage lands in `/var/lib/containers`; rootless Podman
for `coder` is not configured.

For persistent container storage, mount a volume at `/var/lib/containers`.

Nested Podman cgroup management is disabled because the outer privileged
container is the cgroup boundary. Apply CPU and memory limits to the codespace
container itself. The inner default network uses `10.250.0.0/16` to avoid the
outer Podman default network at `10.88.0.0/16`.

Configure the inner Podman network at runtime with:

- `PODMAN_NETWORK_SUBNET`: IPv4 CIDR, default `10.250.0.0/16`
- `PODMAN_NETWORK_GATEWAY`: IPv4 gateway inside the subnet; when omitted, the
  first usable address in the selected subnet is used

Invalid or conflicting values stop the container before sshd starts.
