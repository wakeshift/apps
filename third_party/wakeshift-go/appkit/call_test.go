package appkit_test

import (
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/wakeshift/wakeshift-go/appkit"
	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// WakeArgs, embedded in a wrapped tool's input, adds exactly session_key,
// harness and conversation_id, described as the TypeScript and Python SDKs
// describe them (the harness list comes from internal/harnesses).
func TestWakeArgsSchema(t *testing.T) {
	type in struct {
		Repo string `json:"repo"`
		appkit.WakeArgs
	}
	s, err := jsonschema.For[in](nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"session_key":     "This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it.",
		"harness":         harnesses.ArgDescription(),
		"conversation_id": harnesses.ConversationDescription,
	}
	for name, desc := range want {
		p, ok := s.Properties[name]
		if !ok || p.Description != desc {
			t.Errorf("%s: %+v, want %q", name, p, desc)
		}
	}
	if len(s.Properties) != 4 || !reflect.DeepEqual(s.Required, []string{"repo"}) {
		t.Errorf("properties %v, required %v", s.Properties, s.Required)
	}
}

// A turn OpenClaw runs in Codex's app-server carries a Codex thread, but
// it's OpenClaw's conversation: Codex's connector isn't its wake path.
func TestCodexThreadIsntOpenClaws(t *testing.T) {
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1", "thread_source": "user"}}
	if got := appkit.CodexThread(codex); got != "t1" {
		t.Fatalf("Codex's own thread: %q", got)
	}
	openclaw := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t2", "thread_source": "openclaw"}}
	if got := appkit.CodexThread(openclaw); got != "" {
		t.Fatalf("OpenClaw's turn in Codex taken as a Codex thread: %q", got)
	}
}
