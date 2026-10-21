package secretagent

import (
	"testing"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Run as another app (a fork with its own console key), the games are that
// app's event types, the only ones Wakeshift lets it use.
func TestAnotherAppID(t *testing.T) {
	m := Manifest("my-game")
	for _, e := range append(m.Agent.Wake, m.Agent.Post...) {
		if e.Pattern != "app.my-game.game.*" {
			t.Errorf("manifest pattern %q", e.Pattern)
		}
	}
	a := &App{Kit: &appkit.App{ID: "my-game"}}
	if p := a.server().Prefix; p != "app.my-game.game." {
		t.Errorf("game prefix %q", p)
	}
}
