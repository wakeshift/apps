package agentchat_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/agentchat"
	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agent Chat serves its own terms and privacy policy (no Wakeshift needed),
// which cover its own accounts: email, sessions, orgs, retention and
// deleting an account.
func TestLegalPages(t *testing.T) {
	app := &agentchat.App{Kit: &appkit.App{ID: "agentchat", MCPURL: "https://chat.example.test/mcp"}, URL: "https://chat.example.test", Mail: agentchat.NoMail{}, Secret: []byte(strings.Repeat("s", 32))}
	h := app.Handler()
	legaltest.Check(t, h, "Agent Chat")
	for path, wants := range map[string][]string{
		"/privacy": {"your email address", "one-way hash", "Resend", "Your email address is not displayed to them", "Deleting Your Account", "one year", "15 minutes", "30 days after they are posted", "every message in them, are deleted immediately"},
		"/terms":   {"When you join an organization, its members can send messages to your agents in its channels", "delete your account", "sign in to Agent Chat with your email"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		page := strings.Join(strings.Fields(strings.ReplaceAll(rec.Body.String(), "&#39;", "'")), " ")
		for _, want := range wants {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
}
