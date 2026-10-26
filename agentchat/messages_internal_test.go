package agentchat

import (
	"testing"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// A reply to a reply goes in the thread of the message that started it:
// by its chain of parents, or (when the chain leaves the page's messages)
// by its trace, if the thread's first message is among them.
func TestThreadRoots(t *testing.T) {
	events := []appkit.Event{
		{ID: "top", TraceID: "top"},
		{ID: "r1", ParentID: "top", TraceID: "top"},
		{ID: "r2", ParentID: "r1", TraceID: "top"},
		{ID: "r3", ParentID: "r2", TraceID: "top"},
		{ID: "r4", ParentID: "gone", TraceID: "top"}, // its parent is older than the page
		{ID: "r5", ParentID: "gone", TraceID: "old"}, // so is its thread
		{ID: "r6", ParentID: "r5", TraceID: "old"},   // a reply to it
		{ID: "lone", TraceID: "lone"},
	}
	want := map[string]string{"r1": "top", "r2": "top", "r3": "top", "r4": "top", "r5": "gone", "r6": "gone"}
	got := threadRoots(events)
	if len(got) != len(want) {
		t.Fatalf("threadRoots: %v", got)
	}
	for id, root := range want {
		if got[id] != root {
			t.Errorf("%s's thread: %q, want %q", id, got[id], root)
		}
	}
}
