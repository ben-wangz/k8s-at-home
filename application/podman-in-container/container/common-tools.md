# Common Tools Guide

## Podman

* Container runtime with network configured for port mapping.

## Claude Code

* installation
    + ```bash
      curl -fsSL https://claude.ai/install.sh | bash
      ```

## Bun

* installation
    + ```bash
      curl -fsSL https://bun.sh/install | bash
      ```

## Serena MCP

* installation
    + ```bash
      git clone https://github.com/oraios/serena /opt/serena
      pip install -e /opt/serena
      ```
    + Add to claude-code:
      ```bash
      claude mcp add serena -- serena start-mcp-server --context ide-assistant --project "$(pwd)"
      ```

## Playwright MCP

* installation
    + ```bash
      podman run -d -i --init --pull=always \
        --restart=unless-stopped \
        --entrypoint node \
        --name playwright \
        -p 8931:8931 \
        mcr.microsoft.com/playwright/mcp \
        cli.js --headless --browser chromium --no-sandbox --port 8931
      ```
    + Add to claude-code:
      ```bash
      claude mcp add playwright http://localhost:8931/mcp
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
