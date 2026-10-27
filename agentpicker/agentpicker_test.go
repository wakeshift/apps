package agentpicker

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestOrderAndGroups(t *testing.T) {
	var main, more []string
	for _, a := range Agents {
		if a.Group == "main" {
			main = append(main, a.Name)
		} else {
			more = append(more, a.Name)
		}
	}
	if got := strings.Join(main, ", "); got != "Claude Code, Codex, Claude Cowork, Cursor, opencode" {
		t.Errorf("main row: %s", got)
	}
	if got := strings.Join(more, ", "); got != "Antigravity, ChatGPT, Hermes Agent, OpenClaw, Copilot CLI, Any other MCP client" {
		t.Errorf("more: %s", got)
	}
}

func TestHTML(t *testing.T) {
	o := Options{Name: "nightfall", URL: "https://nightfall.example/mcp?x=1&y=<2>", Ask: `Join a game of "Nightfall".`, Each: "each turn"}
	h := string(HTML(o))
	for _, a := range Agents {
		if !strings.Contains(h, `id="ap-`+a.ID+`"`) || !strings.Contains(h, `data-agent="`+a.ID+`" aria-pressed="false"`) {
			t.Errorf("%s: no panel or button", a.ID)
		}
	}
	for _, want := range []string{
		"claude mcp add --transport http nightfall https://nightfall.example/mcp?x=1&amp;y=&lt;2&gt;",
		"codex mcp add nightfall --url",
		// OAuth: without --auth oauth, hermes mcp add fails its connection test.
		"hermes mcp add nightfall --url https://nightfall.example/mcp?x=1&amp;y=&lt;2&gt; --auth oauth",
		"    url: https://nightfall.example/mcp?x=1&amp;y=&lt;2&gt;\n    auth: oauth",
		"Each turn wakes the agent within seconds",
		"&#34;Nightfall&#34;",
		`aria-expanded="false" aria-controls="ap-more">More agents</button>`,
		"For authentication, choose OAuth.",
		`href="cursor://anysphere.cursor-deeplink/mcp/install?name=nightfall&amp;config=`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"ZgotmplZ", "{name}", "{url}", "{each}", "{Each}", "{auth}", "{hermesauth", "<script"} {
		if strings.Contains(h, bad) {
			t.Errorf("rendered %q", bad)
		}
	}
	// Guest sign-in needs no one: no "/mcp … Authenticate" hints.
	if strings.Contains(h, "Authenticate") {
		t.Error("sign-in hints shown for an app that doesn't need them")
	}
	h2 := string(HTML(Options{Name: "chat", URL: "https://c/mcp", Ask: "Hi.", Interactive: true, SignIn: "Sign in with Wakeshift when it asks.", NoAuth: false}))
	if !strings.Contains(h2, "choose chat, then Authenticate") || strings.Count(h2, "Sign in with Wakeshift when it asks.") != len(Agents) {
		t.Error("interactive sign-in notes missing")
	}
	noAuth := string(HTML(Options{Name: "w", URL: "u", Ask: "a", NoAuth: true}))
	if !strings.Contains(noAuth, "choose No authentication.") {
		t.Error("NoAuth not used")
	}
	// Without sign-in, no OAuth for Hermes: its gateway can't start OAuth itself.
	if !strings.Contains(noAuth, "<code>hermes mcp add w --url u</code>") || strings.Contains(noAuth, "auth: oauth") {
		t.Error("NoAuth: Hermes still asked for OAuth")
	}
}

// Public copy never ranks agents or describes how the listener works.
func TestCopy(t *testing.T) {
	all := strings.ToLower(string(agentsJSON))
	for _, bad := range []string{"popular", "most asked", "monitor tool", "wait loop", "cursor line", "beacon", "sse", "websocket", "stream link"} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(bad) + `\b`).MatchString(all) {
			t.Errorf("agents.json says %q", bad)
		}
	}
}

func TestHandler(t *testing.T) {
	for path, typ := range map[string]string{ScriptPath: "text/javascript", StylePath: "text/css"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), typ) || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent-picker/x", nil))
	if rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}
