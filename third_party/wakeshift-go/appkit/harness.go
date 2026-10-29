package appkit

import "github.com/wakeshift/wakeshift-go/internal/harnesses"

// HarnessAgent is the agent ID a client is named by: its harness's ID
// ("copilot-cli" → copilot, "Claude Code" → claude-code), as Wakeshift names
// it, else its name as an ID (lowercase letters, digits and dashes; "agent"
// if nothing's left): for naming the agent an app adds for an MCP client
// (AddAgent).
func HarnessAgent(client string) string { return harnesses.AgentID(client) }
