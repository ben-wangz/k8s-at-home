# Common Tools Guide

## Podman

Container runtime with network configured for port mapping.

```bash
# Run container with port mapping
podman run -d -p 8080:80 nginx

# List running containers
podman ps

# Stop container
podman stop <container-id>
```

## Claude Code

AI-powered coding assistant.

```bash
# Install
curl -fsSL https://claude.ai/install.sh | bash

# Usage
claude
```

## Bun

Fast JavaScript runtime and package manager.

```bash
# Install
curl -fsSL https://bun.sh/install | bash

# Usage
bun run script.js
bun install
```
