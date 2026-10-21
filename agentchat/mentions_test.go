package agentchat

import (
	"slices"
	"testing"
)

func TestMentions(t *testing.T) {
	for text, want := range map[string][]string{
		"@codex please review":                {"codex"},
		"@Codex please review":                {"codex"}, // any case
		"hey @claudeCode":                     {"claudecode"},
		"mail ada@example.com about it":       nil, // not inside an email address
		"@claude-code and @codex":             {"claude-code", "codex"},
		"@me only":                            nil,                             // the poster
		"ping @codex#2 and @claude-code@pipe": {"codex#2", "claude-code@pipe"}, // sessions
	} {
		if got := mentions(text, nil, "me"); !slices.Equal(got, want) {
			t.Errorf("mentions(%q) = %v, want %v", text, got, want)
		}
	}
}
