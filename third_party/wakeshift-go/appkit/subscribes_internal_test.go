package appkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The shared fixture: a tool that declares its events, as tools/list
// describes it, the same in every SDK.
func TestSubscribesGolden(t *testing.T) {
	if _, err := os.Stat(goldenDir); err != nil {
		t.Skip("no shared golden files here")
	}
	b, err := os.ReadFile(filepath.Join(goldenDir, "subscribes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name, Description, App, Want string
			Events                       []string
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 8 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		if got := withSubscribes(c.Description, c.Events, c.App); got != c.Want {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Want)
		}
	}
}

// The warning and the error name the tool and give the fix, listing what
// the tool declares and the event it subscribed to.
func TestUndeclaredMessage(t *testing.T) {
	news := &eventType{name: "table.news"}
	turn := &eventType{name: "game.turn"}
	cases := []struct {
		decl  *declaration
		isErr bool
		want  string
	}{
		{nil, false, "appkit: the tool watch subscribed to table.news without declaring the events it subscribes to, so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error."},
		{&declaration{events: []*eventType{turn}}, false, "appkit: the tool watch subscribed to table.news, which its Subscribes doesn't list (game.turn), so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<game.turn's event>, <table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error."},
		{&declaration{events: []*eventType{}}, false, "appkit: the tool watch subscribed to table.news, but declares that it subscribes to nothing, so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error."},
		{nil, true, "appkit: the tool watch subscribed to table.news without declaring the events it subscribes to, so the subscribe was refused. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>))."},
	}
	for _, c := range cases {
		if got := undeclaredMessage("watch", news.name, c.decl, c.isErr); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}
