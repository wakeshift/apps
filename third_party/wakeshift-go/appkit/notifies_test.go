package appkit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	unnamedSentence = "This tool is how this app notifies you in this conversation. This app delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."
	shipyardWhen    = "This tool is how Shipyard notifies you in this conversation when a build finishes. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."
)

// A subscribing tool's description gets the sentences, once, after its own,
// naming the app and saying when where given.
func TestNotifies(t *testing.T) {
	if got := Notifies("Watch a build until it finishes. "); got != "Watch a build until it finishes. "+unnamedSentence {
		t.Fatalf("%q", got)
	}
	opts := []NotifyOption{NotifyApp("Shipyard"), NotifyWhen("a build finishes.")}
	if got := Notifies(Notifies("Watch a build.", opts...), opts...); got != "Watch a build. "+shipyardWhen {
		t.Fatalf("twice: %q", got)
	}
	if got := Notifies("", NotifyApp(" ")); got != unnamedSentence {
		t.Fatalf("empty: %q", got)
	}
}

// tools/list names the app in Notifies' sentences as the console has it,
// asking Wakeshift once, whatever the when; a tool that named one, or
// another tool, is as it was; with no name from Wakeshift they keep "this
// app".
func TestNotifiesNamesTheApp(t *testing.T) {
	var asked atomic.Int32
	app, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/apps/v1/app" {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"app_test","name":"Shipyard"}`)
	}))
	at := &Attachment{app: app}
	list := func() *mcp.ListToolsResult {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "watch_build", Description: Notifies("Watch a build.", NotifyWhen("a build finishes"))},
			{Name: "watch_named", Description: Notifies("Watch a build.", NotifyApp("Acme CI"))},
			{Name: "list_builds", Description: "List builds."},
		}}
	}
	l := list()
	at.nameApp(context.Background(), l)
	if l.Tools[0].Description != "Watch a build. "+shipyardWhen || l.Tools[1].Description != "Watch a build. "+notifiesSentence("Acme CI", "") || l.Tools[2].Description != "List builds." {
		t.Fatalf("%q / %q / %q", l.Tools[0].Description, l.Tools[1].Description, l.Tools[2].Description)
	}
	at.nameApp(context.Background(), list())
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
	app2, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"app_test","name":""}`)
	}))
	l = list()
	(&Attachment{app: app2}).nameApp(context.Background(), l)
	if l.Tools[0].Description != "Watch a build. "+notifiesSentence(unnamedApp, "a build finishes") {
		t.Fatalf("unnamed: %q", l.Tools[0].Description)
	}
}
