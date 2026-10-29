// harnessAgent names the agent an MCP client signs in as: its
// harness's ID ("copilot-cli" → copilot, "Claude Code" → claude-code), as
// Wakeshift names it, else its name as an ID. A port
// of the core's harnesses.AgentID: test/agent-ids.json is the Go
// function's output, and both sides' tests check it.

interface Known {
  id: string;
  clients: string[];
  /** Its client name is one others send too (Hermes Agent's "mcp"): it keeps its own ID. */
  unsure?: boolean;
}

// The core's harness table (internal/harnesses): only what naming needs.
const known: Known[] = [
  { id: "codex", clients: ["codex-mcp-client"] },
  { id: "claude-code", clients: ["claude-code"] },
  { id: "cursor", clients: ["Cursor", "cursor-vscode"] },
  { id: "cursor-cloud", clients: [] },
  { id: "grok-bot", clients: [] },
  { id: "copilot", clients: ["copilot-cli"] },
  { id: "opencode", clients: ["opencode"] },
  { id: "antigravity", clients: ["antigravity-client"] },
  { id: "openclaw", clients: ["openclaw-bundle-mcp"] },
  { id: "hermes", clients: ["mcp"], unsure: true },
  { id: "chatgpt", clients: ["openai-mcp"] },
  { id: "gemini", clients: ["gemini-cli-mcp-client"] },
  { id: "goose", clients: ["goose-cli", "goose-desktop"] },
  { id: "cline", clients: ["Cline"] },
  { id: "amp", clients: [] },
  { id: "devin", clients: [] },
  { id: "vscode", clients: ["Visual Studio Code", "Visual Studio Code - Insiders"] },
];

/**
 * The agent ID an MCP client's name maps to: its harness's ID, else the
 * name as an ID (2–40 lowercase letters, digits and dashes; "agent" if
 * nothing's left).
 */
export function harnessAgent(client: string): string {
  const name = client.trim().toLowerCase();
  if (name) {
    for (const h of known) {
      if (h.clients.some((c) => c.toLowerCase() === name)) {
        if (!h.unsure) return h.id;
        break;
      }
    }
  }
  const id = slug(client);
  for (const h of known) {
    if (h.unsure) continue;
    if (id === h.id || id.startsWith(h.id + "-")) return h.id;
    if (h.clients.some((c) => id === slug(c))) return h.id;
  }
  return id;
}

// slug is a name as an agent ID: lowercase letters, digits and dashes, up
// to 40 characters, without anything in parentheses ("agent" if fewer than
// 2 are left).
function slug(name: string): string {
  let n = name.toLowerCase();
  const i = n.indexOf("(");
  if (i > 0) n = n.slice(0, i);
  let out = "";
  let dash = false;
  for (const r of n.trim()) {
    if ((r >= "a" && r <= "z") || (r >= "0" && r <= "9")) {
      out += r;
      dash = false;
    } else if (!dash && out.length > 0) {
      out += "-";
      dash = true;
    }
  }
  let id = out.replace(/^-+|-+$/g, "");
  if (id.length > 40) id = id.slice(0, 40).replace(/^-+|-+$/g, "");
  return id.length < 2 ? "agent" : id;
}
