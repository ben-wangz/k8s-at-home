#!/usr/bin/env node
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { isAbsolute, resolve } from "node:path";
import { Readable, Writable } from "node:stream";

const DEFAULT_SDK_PATH = "/opt/nodejs/lib/node_modules/@agentclientprotocol/claude-agent-acp/node_modules/@agentclientprotocol/sdk/dist/acp.js";

function usage() {
  return `Usage: prompt-claude-agent-acp.mjs --prompt <text> [options]

Options:
  --prompt <text>                    Prompt text to send to Claude Code.
  --prompt-file <path>               Read prompt text from a UTF-8 file.
  --cwd <path>                       Session working directory. Defaults to current directory.
  --command <path-or-name>           claude-agent-acp command. Defaults to claude-agent-acp.
  --sdk-path <path>                  ACP SDK acp.js path. Defaults to the global install path used in this workspace.
  --model <value>                    Set ACP session config option model before prompting.
  --mode <value>                     Set ACP session config option mode before prompting.
  --effort <value>                   Set ACP session config option effort before prompting.
  --auto-approve-first-permission    Select the first permission option when Claude requests approval.
  --help                             Show this help.

Security:
  The script inherits environment variables from the parent process, but does not load or print .env.
  Permission requests are denied unless --auto-approve-first-permission is set.`;
}

function parseArgs(argv) {
  const options = {
    command: "claude-agent-acp",
    cwd: process.cwd(),
    sdkPath: DEFAULT_SDK_PATH,
    autoApproveFirstPermission: false,
  };

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`Missing value for ${arg}`);
      return argv[i];
    };

    switch (arg) {
      case "--prompt":
        options.prompt = next();
        break;
      case "--prompt-file":
        options.promptFile = next();
        break;
      case "--cwd":
        options.cwd = next();
        break;
      case "--command":
        options.command = next();
        break;
      case "--sdk-path":
        options.sdkPath = next();
        break;
      case "--model":
        options.model = next();
        break;
      case "--mode":
        options.mode = next();
        break;
      case "--effort":
        options.effort = next();
        break;
      case "--auto-approve-first-permission":
        options.autoApproveFirstPermission = true;
        break;
      case "--help":
        options.help = true;
        break;
      default:
        throw new Error(`Unknown option: ${arg}`);
    }
  }

  return options;
}

async function loadPrompt(options) {
  if (options.prompt && options.promptFile) {
    throw new Error("Use only one of --prompt or --prompt-file");
  }
  if (options.prompt) return options.prompt;
  if (options.promptFile) return readFile(options.promptFile, "utf8");
  throw new Error("Missing required --prompt or --prompt-file");
}

function absoluteExistingDirectory(path) {
  const cwd = isAbsolute(path) ? path : resolve(path);
  if (!existsSync(cwd)) throw new Error(`Directory does not exist: ${cwd}`);
  return cwd;
}

class Client {
  constructor(options) {
    this.options = options;
  }

  async sessionUpdate(params) {
    const update = params.update;
    switch (update.sessionUpdate) {
      case "agent_message_chunk":
        if (update.content.type === "text") process.stdout.write(update.content.text);
        break;
      case "tool_call":
        console.error(`[tool_call] ${update.title} (${update.status})`);
        break;
      case "tool_call_update":
        console.error(`[tool_call_update] ${update.toolCallId} (${update.status})`);
        break;
      case "plan":
      case "agent_thought_chunk":
      case "user_message_chunk":
        break;
      default:
        console.error(`[session_update] ${update.sessionUpdate}`);
        break;
    }
  }

  async requestPermission(params) {
    console.error(`[permission] ${params.toolCall.title}`);
    if (this.options.autoApproveFirstPermission && params.options.length > 0) {
      const option = params.options[0];
      console.error(`[permission] selected first option: ${option.name}`);
      return { outcome: { outcome: "selected", optionId: option.optionId } };
    }
    console.error("[permission] denied by default; rerun with --auto-approve-first-permission only if safe");
    return { outcome: { outcome: "cancelled" } };
  }
}

async function main() {
  const options = parseArgs(process.argv.slice(2));
  if (options.help) {
    console.log(usage());
    return;
  }

  const [acp, prompt] = await Promise.all([
    import(options.sdkPath),
    loadPrompt(options),
  ]);
  const cwd = absoluteExistingDirectory(options.cwd);
  const child = spawn(options.command, [], {
    cwd,
    stdio: ["pipe", "pipe", "inherit"],
    env: process.env,
  });

  child.on("error", (error) => {
    console.error(`[child] ${error.message}`);
  });

  const client = new Client(options);
  const stream = acp.ndJsonStream(Writable.toWeb(child.stdin), Readable.toWeb(child.stdout));
  const connection = new acp.ClientSideConnection(() => client, stream);

  try {
    await connection.initialize({
      protocolVersion: acp.PROTOCOL_VERSION,
      clientCapabilities: {
        fs: { readTextFile: true, writeTextFile: true },
        auth: { terminal: true },
        _meta: { "terminal-auth": true },
      },
    });

    const session = await connection.newSession({ cwd, mcpServers: [] });
    for (const [configId, value] of Object.entries({ model: options.model, mode: options.mode, effort: options.effort })) {
      if (value) await connection.setSessionConfigOption({ sessionId: session.sessionId, configId, value });
    }

    const result = await connection.prompt({
      sessionId: session.sessionId,
      prompt: [{ type: "text", text: prompt }],
    });
    console.error(`\n[done] ${result.stopReason}`);
  } finally {
    child.kill();
  }
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
