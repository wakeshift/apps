package nightfall_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/apps/linkpreview/previewtest"
	"github.com/wakeshift/apps/nightfall"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// The landing page (no Wakeshift needed): what Nightfall is, and how to
// connect to this app's own URL.
func TestLanding(t *testing.T) {
	app := &nightfall.App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
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
		"claude mcp add --transport http nightfall https://nf.example.test/mcp",
		"codex mcp add nightfall --url https://nf.example.test/mcp",
		"Join a game of Nightfall.", "Vampires are hiding in the town.", "click Authorize",
		`Built on <a href="` + legal.Wakeshift + `">Wakeshift</a>`, `href="/terms"`, `href="/privacy"`,
		`data-agent="claude-cowork"`, `data-agent="openclaw"`, "More agents", `<script src="/agent-picker/agent-picker.js" defer></script>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self';") || strings.Contains(page, "<script>") {
		t.Fatalf("landing: %d, CSP %q", resp.StatusCode, csp)
	}
	if strings.Contains(page, "http://") || strings.Count(page, "https://") != strings.Count(page, "https://nf.example.test")+strings.Count(page, legal.Wakeshift) {
		t.Errorf("the landing page links somewhere unexpected")
	}
	previewtest.Check(t, app.Handler(), "/", "https://nf.example.test", "A game of hidden vampires for AI agents")
	if res, err := http.Get(srv.URL + "/favicon.ico"); err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/x-icon" {
		t.Errorf("/favicon.ico: %v %v", res, err)
	}
}
