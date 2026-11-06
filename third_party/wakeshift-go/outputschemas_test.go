package wakeshift_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go"
)

// Attach's own tools answer with structuredContent matching their output
// schemas, with Wakeshift's text in its wakeshift field; a refusal has none.
func TestBuiltinToolsStructuredContent(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	sub := w.callTool(t, "ada", "", "subscribe_events", map[string]any{"event": "table.news", "arguments": map[string]any{"table": "oak"}}, nil)
	if sub.IsError || sub.RPCError != nil {
		t.Fatalf("subscribe_events: %+v", sub)
	}
	if want := map[string]any{"subscribed": map[string]any{"event": "table.news", "arguments": map[string]any{"table": "oak"}, "created": true}}; !reflect.DeepEqual(sub.Structured, want) {
		t.Fatalf("subscribe_events' structuredContent: %v", sub.Structured)
	}
	unsub := w.callTool(t, "ada", "", "unsubscribe_events", map[string]any{"event": "table.news", "arguments": map[string]any{"table": "oak"}}, nil)
	if want := map[string]any{"ended": true, "wakeshift": "Keep passing session_key."}; !reflect.DeepEqual(unsub.Structured, want) {
		t.Fatalf("unsubscribe_events' structuredContent: %v", unsub.Structured)
	}
	byID := w.callTool(t, "ada", "", "unsubscribe_events", map[string]any{"id": "es_1"}, nil)
	if want := map[string]any{"ended": true}; !reflect.DeepEqual(byID.Structured, want) {
		t.Fatalf("unsubscribe_events by id: %v", byID.Structured)
	}
	none := w.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil)
	if want := map[string]any{"subscriptions": []any{}}; !reflect.DeepEqual(none.Structured, want) {
		t.Fatalf("list_subscriptions, none: %v", none.Structured)
	}
	w.core.native = []wakeshift.EventSubscription{{ID: "es_1", Event: "table.news", Arguments: []byte(`{"table":"oak"}`), Destination: "webhook",
		WebhookHost: "x.test", CreatedAt: time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)}}
	list := w.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil)
	want := map[string]any{"subscriptions": []any{map[string]any{"id": "es_1", "event": "table.news", "arguments": map[string]any{"table": "oak"},
		"destination": "webhook", "since": "2026-11-04T01:02:03Z"}}}
	if !reflect.DeepEqual(list.Structured, want) {
		t.Fatalf("list_subscriptions: %v", list.Structured)
	}
	refused := w.callTool(t, "ada", "", "subscribe_events", map[string]any{"event": "no.such"}, nil)
	if !refused.IsError || refused.Structured != nil {
		t.Fatalf("a refusal: %+v", refused)
	}
}
