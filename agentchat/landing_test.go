package agentchat_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/agentchat"
	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// The landing page (no Wakeshift needed): what Agent Chat is, and how to
// connect to this app's own URL.
func TestLanding(t *testing.T) {
	app := &agentchat.App{Kit: &appkit.App{ID: "agentchat", MCPURL: "https://chat.example.test/mcp"}, URL: "https://chat.example.test", Mail: agentchat.NoMail{}, Secret: []byte(strings.Repeat("s", 32))}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	for _, want := range []string{
		"claude mcp add --transport http agent-chat https://chat.example.test/mcp",
		"codex mcp add agent-chat --url https://chat.example.test/mcp",
		"List my channels in Agent Chat.", "invite code", `href="/signin"`,
		// The agent picker: every agent, and how this one signs in.
		`data-agent="claude-cowork"`, `data-agent="chatgpt"`, `data-agent="copilot"`, "More agents",
		"type /mcp in Claude Code, choose agent-chat, then Authenticate", "sign in to Agent Chat with your email",
		`<script src="/agent-picker/agent-picker.js" defer></script>`,
		`Built on <a href="` + legal.Wakeshift + `">Wakeshift</a>`, `href="/terms"`, `href="/privacy"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	// Scripts only from this origin (the agent picker's), never inline.
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self';") || strings.Contains(page, "<script>") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("landing: %d, CSP %q", resp.StatusCode, csp)
	}
	if strings.Contains(page, "http://") || strings.Count(page, "https://") != strings.Count(page, "https://chat.example.test")+strings.Count(page, legal.Wakeshift) {
		t.Errorf("the landing page links somewhere unexpected")
	}

	// Only / itself: other paths stay not found.
	resp, err = http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/nope: %d", resp.StatusCode)
	}
}
