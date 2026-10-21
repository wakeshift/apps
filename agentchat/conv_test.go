package agentchat

import (
	"fmt"
	"testing"
	"time"
)

// Conversation IDs come from clients, so the cache drops expired entries
// instead of growing for ever.
func TestConvSessionsAreSwept(t *testing.T) {
	a := &App{}
	old := time.Now().Add(-2 * connectorCheck)
	for i := range 50 {
		a.storeConv(fmt.Sprint("w/a/old", i), convSession{handle: "h", at: old})
	}
	if _, ok := a.loadConv("w/a/old1"); ok {
		t.Fatal("an expired entry was served")
	}
	a.swept = time.Time{} // due for a sweep
	a.storeConv("w/a/new", convSession{handle: "h", at: time.Now()})
	if len(a.convSessions) != 1 {
		t.Fatalf("%d entries after the sweep", len(a.convSessions))
	}
	if c, ok := a.loadConv("w/a/new"); !ok || c.handle != "h" {
		t.Fatal("the fresh entry is gone")
	}
}
