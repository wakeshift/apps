package slackline

import (
	"strings"
	"testing"
)

// Every page renders with Slackline's one stylesheet (inline, under the
// CSP) and its dark colour scheme.
func TestPagesRender(t *testing.T) {
	views := map[string]any{
		"landing": nil,
		"home":    homeView{User: User{Name: "Ada"}, Installed: true, Labels: map[string]string{}},
		"consent": consentView{Req: authRequest{Client: client{Name: "Claude Code"}}, User: User{Name: "Ada"}, Label: "laptop"},
		"message": messageView{Text: "Done."},
	}
	for page, data := range views {
		var b strings.Builder
		if err := pages.ExecuteTemplate(&b, page, pageData{CSRF: "c", MCPURL: "https://slackline.example/mcp", Wakeshift: "https://wakeshift.example", Data: data}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		html := b.String()
		for _, want := range []string{`<meta name="color-scheme" content="dark">`, "--pale:#B5EAB4", "<footer class=\"foot\">", `<a href="https://wakeshift.example">Wakeshift</a>, which Slackline is built on`} {
			if !strings.Contains(html, want) {
				t.Errorf("%s: no %s", page, want)
			}
		}
		if n := strings.Count(html, "<style>"); n != 1 {
			t.Errorf("%s: %d stylesheets", page, n)
		}
	}
}

// Each step where people agree to Slackline's terms says so, with links to
// them: Add to Slack and Sign in with Slack on the landing page, Add to
// Slack on a home page whose workspace lacks Slackline, the step that
// confirms where messages go (where a first sign-in lands), and Connect on
// the consent page.
func TestAgreementLine(t *testing.T) {
	const line = `By continuing, you agree to Slackline's <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>.`
	render := func(page string, data any) string {
		var b strings.Builder
		if err := pages.ExecuteTemplate(&b, page, pageData{CSRF: "c", MCPURL: "https://slackline.example/mcp", Wakeshift: "https://wakeshift.example", Data: data}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		return b.String()
	}
	for _, c := range []struct {
		name, page string
		data       any
		want       int
	}{
		{"landing: both buttons, and the closing Add to Slack", "landing", nil, 2},
		{"home, not added, not confirmed", "home", homeView{User: User{Name: "Ada"}, Labels: map[string]string{}}, 2},
		{"home, added, not confirmed", "home", homeView{User: User{Name: "Ada"}, Installed: true, Labels: map[string]string{}}, 1},
		{"home, added and confirmed", "home", homeView{User: User{Name: "Ada", Understood: true}, Installed: true, Labels: map[string]string{}}, 0},
		{"consent, first agent", "consent", consentView{Req: authRequest{Client: client{Name: "Claude Code"}}, User: User{Name: "Ada"}, Label: "laptop", Installed: true}, 1},
		{"consent, later agent", "consent", consentView{Req: authRequest{Client: client{Name: "Claude Code"}}, User: User{Name: "Ada", Understood: true}, Label: "laptop", Installed: true}, 1},
	} {
		html := render(c.page, c.data)
		if n := strings.Count(html, line); n != c.want {
			t.Errorf("%s: the agreement line %d times, want %d", c.name, n, c.want)
		}
	}
	// On the consent page it's by Connect.
	html := render("consent", consentView{Req: authRequest{Client: client{Name: "Claude Code"}}, User: User{Name: "Ada"}, Label: "laptop"})
	if i, j := strings.Index(html, `value="allow">Connect</button>`), strings.Index(html, line); i < 0 || j < i {
		t.Errorf("consent: the agreement line isn't after Connect")
	}
}
