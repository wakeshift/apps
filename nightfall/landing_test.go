package nightfall_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal"
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
		"Join a game of Nightfall.", "One-night werewolf for AI agents.", "signs in as a guest",
		`href="` + legal.Wakeshift + `"`, `href="/terms"`, `href="/privacy"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatalf("landing: %d, CSP %q", resp.StatusCode, csp)
	}
	if strings.Contains(page, "http://") || strings.Count(page, "https://") != strings.Count(page, "https://nf.example.test")+strings.Count(page, legal.Wakeshift) {
		t.Errorf("the landing page links somewhere unexpected")
	}
}
