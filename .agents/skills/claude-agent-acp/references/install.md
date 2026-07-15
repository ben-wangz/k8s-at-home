# Claude Agent ACP Install

## Official Sources

- Repository: `https://github.com/agentclientprotocol/claude-agent-acp`
- NPM package: `@agentclientprotocol/claude-agent-acp`
- Binary: `claude-agent-acp`

## Install

Install globally when you want the binary on `PATH`:

```bash
npm install -g @agentclientprotocol/claude-agent-acp
```

Or run without a global install:

```bash
npx -y @agentclientprotocol/claude-agent-acp
```

After installing, verify the binary resolves:

```bash
command -v claude-agent-acp
```

The package depends on the Claude Agent SDK, which provides the Claude Code native binary through platform-specific optional dependencies. If the native binary is missing, reinstall without omitting optional dependencies.

## Authentication

The adapter supports terminal-based Claude authentication flows through its `--cli` mode.

Claude subscription login:

```bash
claude-agent-acp --cli auth login --claudeai
```

Anthropic Console login:

```bash
claude-agent-acp --cli auth login --console
```

For remote environments where browser redirects are not practical, run the CLI login flow in the terminal session exposed by the host/client.

Do not print tokens or credential files. If using a gateway, pass credentials through environment variables, credential helpers, or client-side auth metadata rather than command-line literals.

## Install Verification

Check that the ACP server can be launched by an ACP client. Running `claude-agent-acp` directly starts a stdio server and waits for ACP messages, so it is normal for the process to appear idle.

For a real end-to-end check, use an ACP client that can spawn `claude-agent-acp` and send `initialize`, `session/new`, and `session/prompt`.
