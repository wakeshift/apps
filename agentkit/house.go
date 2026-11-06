package agentkit

import "github.com/wakeshift/apps/internal/harnesses"

// HarnessAgent is the agent ID a client is named by: its harness's ID
// ("copilot-cli" → copilot, "Claude Code" → claude-code), as Wakeshift names
// it, else its name as an ID (lowercase letters, digits and dashes; "agent"
// if nothing's left): for naming the agent an app adds for an MCP client
// (AddAgent).
func HarnessAgent(client string) string { return harnesses.AgentID(client) }

// CodexThread is the Codex thread an MCP tool call came from, if any, from
// the call's _meta: Codex sends x-codex-turn-metadata.thread_id (0.160+),
// or threadId (earlier). A turn another harness runs in Codex's app-server
// (OpenClaw marks it x-codex-turn-metadata.thread_source "openclaw") isn't
// a Codex thread's: its conversation is that harness's, which Codex's
// connector doesn't wake, so it's "".
func CodexThread(meta map[string]any) string {
	for _, h := range harnesses.All() {
		if harnesses.Embedded(h, meta) {
			return ""
		}
	}
	id, _ := meta["threadId"].(string)
	if turn, ok := meta["x-codex-turn-metadata"].(map[string]any); ok {
		if t, _ := turn["thread_id"].(string); t != "" {
			id = t
		}
	}
	if len(id) > 200 {
		return ""
	}
	return id
}

// ConnectorMetaKey is where a tool result carries a connector beacon.
const ConnectorMetaKey = "wakeshift/connector"

// TaskIDPrefix starts every Wakeshift task ID: tasks/get and tasks/cancel
// for any other ID aren't Wakeshift's.
const TaskIDPrefix = "wkt1_"

// TasksExtension is the MCP tasks extension's capability key.
const TasksExtension = "io.modelcontextprotocol/tasks"
