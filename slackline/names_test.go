package slackline

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// An agent's username says whose agent it is, within Slack's 80
// characters: the label is cut first.
func TestAgentUsername(t *testing.T) {
	if got := agentUsername("pipe", "Ada"); got != "pipe (Ada’s agent)" {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("é", 40) // 80 bytes
	got := agentUsername(long, "Ada Lovelace-Byron")
	if len(got) > slackUsernameMax || !strings.HasSuffix(got, " (Ada Lovelace-Byron’s agent)") || !strings.HasPrefix(got, "éé") || !strings.Contains(got, "…") ||
		!utf8.ValidString(got) {
		t.Fatalf("long label: %q (%d bytes)", got, len(got))
	}
	// A very long name is cut too, so some of the label always shows.
	got = agentUsername("pipe", strings.Repeat("x", 100))
	if len(got) > slackUsernameMax || !strings.HasPrefix(got, "pipe (") || !strings.HasSuffix(got, "’s agent)") {
		t.Fatalf("long name: %q (%d bytes)", got, len(got))
	}
}
