package appkit

import (
	"fmt"
	"testing"
	"time"
)

// Thread IDs come from clients: the connector-state cache drops expired
// entries rather than growing for ever.
func TestConnectorStateCacheIsSwept(t *testing.T) {
	var c stateCache
	for i := range 50 {
		c.Store(fmt.Sprint("w/a/", i), connectorState{at: time.Now().Add(-time.Minute)})
	}
	c.swept = time.Time{}
	c.Store("w/a/new", connectorState{handle: "h", at: time.Now()})
	if n := c.len(); n != 1 {
		t.Fatalf("%d entries after the sweep", n)
	}
	if st, ok := c.Load("w/a/new"); !ok || st.handle != "h" {
		t.Fatal("the fresh entry is gone")
	}
}
