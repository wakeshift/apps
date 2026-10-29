package agentchat

import (
	"strings"
	"testing"

	"github.com/wakeshift/apps/email/emailtest"
)

// The sign-in link's email, as sent: go test -run TestSignInMail -update
// rewrites testdata/, which shows it in a browser.
func TestSignInMail(t *testing.T) {
	const link = "https://chat.wakeshift.ai/signin/link#abc_DEF-123"
	m := signInMail("ada@example.com", link, "https://chat.wakeshift.ai")
	if m.To != "ada@example.com" || m.Subject != "Sign in to Agent Chat" {
		t.Fatalf("to %q, subject %q", m.To, m.Subject)
	}
	emailtest.Golden(t, "signin", m.HTML, m.Text, link)
	for _, want := range []string{"It works once, for the next 15 minutes.", "If you didn't ask to sign in, ignore this email.", "never asks for a password"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("the text has no %q", want)
		}
	}
	// Agent Chat's own look: its name and violet, no Wakeshift logo.
	if !strings.Contains(m.HTML, ">Agent Chat</td>") || !strings.Contains(m.HTML, "#7b5cff") || strings.Contains(m.HTML, "<img") {
		t.Error("the HTML isn't Agent Chat's")
	}
	// The tests' fake mail picks out the first sign-in link in the text:
	// it must be the only one.
	if strings.Count(m.Text, "/signin/link#") != 1 {
		t.Error("the text should carry the link once")
	}
	if linkTTL.Minutes() != 15 {
		t.Error("the email says the link works for 15 minutes")
	}
}
