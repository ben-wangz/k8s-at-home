# codespace

`codespace` is an Ubuntu 24.04 development container with SSH access and a
Podman-in-container runtime.

Run it as a privileged container so the nested Podman runtime can create
namespaces, mounts, and devices.

Preinstalled components include:

- Go 1.26.5, Node.js 24.13.1, and Python 3
- kubectl 1.35.4 and Helm 4.1.4
- Codex and Grok development CLIs
- Playwright 1.61.1 with Chromium and its system dependencies
- poppler-utils, pdftk, and img2pdf
- Common development, terminal, archive, and network diagnostic tools
- English and Simplified Chinese UTF-8 locales
- Automatic `/root/.bash_profile` creation that loads an existing `.bashrc`

## Build

```bash
podman build \
  --file application/codespace/container/Containerfile \
  --tag localhost/k8s-at-home-codespace-base:dev \
  .
```

The default build uses the project's DaoCloud proxy for the Ubuntu 24.04 base
image. Override `UBUNTU_IMAGE` if direct Docker Hub access is preferred. The Go
archive is downloaded directly from `https://go.dev/dl`.

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

Connect over SSH:

```bash
ssh -p 2222 root@127.0.0.1
```

Then verify the nested runtime:

```bash
podman run --rm docker.io/library/alpine:latest \
  echo "Hello from Podman in codespace"
```

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
