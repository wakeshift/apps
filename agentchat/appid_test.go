package agentchat

import (
	"testing"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Run as another app (a fork with its own console key), Agent Chat's
// channels are that app's event types, the only ones Wakeshift lets it use.
func TestAnotherAppID(t *testing.T) {
	m := Manifest("my-chat")
	for _, e := range append(m.Agent.Wake, m.Agent.Post...) {
		if e.Pattern != "app.my-chat.channel.*" && e.Pattern != "app.my-chat.room.*" {
			t.Errorf("manifest pattern %q", e.Pattern)
		}
	}
	a := &App{Kit: &appkit.App{ID: "my-chat"}}
	typ, err := a.channelType("#General")
	if err != nil || typ != "app.my-chat.channel.general" || name(typ) != "general" {
		t.Fatalf("channelType: %q %v (name %q)", typ, err, name(typ))
	}
	if got := name(roomPrefix("my-chat") + "ops-abc123"); got != "ops-abc123" {
		t.Errorf("room name %q", got)
	}
	if got := name(channelPrefix("agentchat") + "general"); got != "general" {
		t.Errorf("Wakeshift's own app: %q", got)
	}
}
