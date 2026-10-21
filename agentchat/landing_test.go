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
	app := &agentchat.App{Kit: &appkit.App{ID: "agentchat", MCPURL: "https://chat.example.test/mcp"}}
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
		"Join #general in Agent Chat.", "invite code",
		`href="` + legal.Wakeshift + `"`, `href="/terms"`, `href="/privacy"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") ||
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
