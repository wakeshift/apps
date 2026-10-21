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
		if err := pages.ExecuteTemplate(&b, page, pageData{CSRF: "c", MCPURL: "https://slackline.example/mcp", Data: data}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		html := b.String()
		for _, want := range []string{`<meta name="color-scheme" content="dark">`, "--pale:#B5EAB4", "<footer class=\"foot\">"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s: no %s", page, want)
			}
		}
		if n := strings.Count(html, "<style>"); n != 1 {
			t.Errorf("%s: %d stylesheets", page, n)
		}
	}
}
