package event

import "testing"

// ChatGPT's MCP client is openai-mcp, by its client name or its
// User-Agent; Work chats send events/* and tools/call as "openai-mcp/1.0.0
// (Codex)" (the voucher probe, November 5, 2026), events/list as
// "openai-mcp/1.0.0".
func TestDialectOf(t *testing.T) {
	for _, c := range []struct{ client, ua, want string }{
		{"openai-mcp", "", DialectChatGPT},
		{"", "openai-mcp/1.0.0", DialectChatGPT},
		{"", "openai-mcp/1.0.0 (Codex)", DialectChatGPT},
		{"", "codex-mcp-client/0.166.0", DialectDraft},
		{"claude-code", "claude-code/2.1.316 (cli)", DialectDraft},
	} {
		if got := DialectOf(c.client, c.ua); got != c.want {
			t.Errorf("%q %q: %s, want %s", c.client, c.ua, got, c.want)
		}
	}
}
