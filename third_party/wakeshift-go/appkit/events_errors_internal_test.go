package appkit

import (
	"testing"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// A user at the limit of live webhook subscriptions gets the dialect's
// ResourceExhausted (InvalidParams for ChatGPT, which has none), with
// Wakeshift's message saying what to do.
func TestSubscriptionLimitIsResourceExhausted(t *testing.T) {
	err := &APIError{Status: 403, Code: "subscription_limit", Message: "this user already has 20 live webhook subscriptions to this app"}
	for dialect, want := range map[string]int{event.DialectDraft: -32025, event.DialectChatGPT: event.InvalidParams} {
		code, msg, data := rpcErrorOf(err, event.CodesFor(dialect))
		if code != want || msg != err.Message {
			t.Errorf("%s: %d %q, want %d", dialect, code, msg, want)
		}
		if d, _ := data.(map[string]string); d["kind"] != "subscriptions" {
			t.Errorf("%s: data %v", dialect, data)
		}
	}
}
