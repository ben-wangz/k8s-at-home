# Common Tools Guide

## Podman

* Container runtime with network configured for port mapping.

## OpenCode

* installation
    + ```bash
      podman run --rm -it \
        --name opencode-server \
        -p 8080:8080 \
        -v ~/.config/opencode/opencode.json:/root/.config/opencode/opencode.json:ro \
        -v $HOME/code:$HOME/code \
        ghcr.io/anomalyco/opencode:1.2.27 \
        --hostname 0.0.0.0 --port 8080 web
      ```
* open `http://localhost:8080`

## Bun

* installation
    + ```bash
      curl -fsSL https://bun.sh/install | bash
      ```

## Serena MCP

* installation
    + ```bash
      podman run -d \
        --name serena \
        --pull=always \
        -v $HOME/code:$HOME/code \
        ghcr.io/oraios/serena:latest \
        sleep infinity
      ```

## Playwright MCP

* installation
    + ```bash
      podman run -d -i --init --pull=always \
        --restart=unless-stopped \
        --entrypoint node \
        --name playwright \
        -p 8931:8931 \
        mcr.microsoft.com/playwright/mcp:latest \
        cli.js --headless --browser chromium --no-sandbox --port 8931
      ```

## Paper Search MCP

* installation
    + ```bash
      pip install paper-search-mcp
      ```

## ossutil

* installation
    + ```bash
      curl -fsSL "https://gosspublic.alicdn.com/ossutil/1.7.19/ossutil-v1.7.19-linux-amd64.zip" -o /tmp/ossutil.zip && \
      unzip -q /tmp/ossutil.zip ossutil-v1.7.19-linux-amd64/ossutil64 -d /tmp && \
      mkdir -p $HOME/bin && \
      mv /tmp/ossutil-v1.7.19-linux-amd64/ossutil64 $HOME/bin/ && \
      rm -rf /tmp/ossutil.zip /tmp/ossutil-v1.7.19-linux-amd64
      ```
