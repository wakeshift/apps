package agentkit

import "testing"

// A turn OpenClaw runs in Codex's app-server carries a Codex thread, but
// it's OpenClaw's conversation: Codex's connector isn't its wake path.
func TestCodexThreadIsntOpenClaws(t *testing.T) {
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1", "thread_source": "user"}}
	if got := CodexThread(codex); got != "t1" {
		t.Fatalf("Codex's own thread: %q", got)
	}
	openclaw := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t2", "thread_source": "openclaw"}}
	if got := CodexThread(openclaw); got != "" {
		t.Fatalf("OpenClaw's turn in Codex taken as a Codex thread: %q", got)
	}
}
