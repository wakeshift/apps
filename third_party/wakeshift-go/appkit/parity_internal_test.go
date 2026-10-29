package appkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

// What the Go SDK does as the TypeScript and Python SDKs do.

type filterArgs struct {
	Channel string `json:"channel"`
	Kind    string `json:"kind,omitempty"`
}

type filterPayload struct {
	Kind string `json:"kind"`
	Room string `json:"room"`
}

// A Filter given without a Topic is kept, with the default route's filter on
// each optional argument added to it.
func TestFilterWithTheDefaultRoute(t *testing.T) {
	app := &App{}
	ev, err := DefineEvent(app, "chat.message", EventDef[filterArgs, filterPayload]{
		Description: "A message",
		Text:        func(p filterPayload, a filterArgs) string { return p.Kind },
		Filter:      map[string]string{"room": "{channel}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := ev.t.manifestEntry()
	if m.Topic != "ev/chat.message/{channel}" || !reflect.DeepEqual(m.Filter, map[string]string{"room": "{channel}", "kind": "{kind}"}) {
		t.Errorf("topic %q filter %v", m.Topic, m.Filter)
	}

	// Without one, only the default's.
	ev2, err := DefineEvent(app, "chat.other", EventDef[filterArgs, filterPayload]{
		Description: "Another", Text: func(p filterPayload, a filterArgs) string { return p.Kind },
	})
	if err != nil {
		t.Fatal(err)
	}
	if f := ev2.t.manifestEntry().Filter; !reflect.DeepEqual(f, map[string]string{"kind": "{kind}"}) {
		t.Errorf("filter %v", f)
	}
}

// CheckAgent asks Wakeshift for the agent's token each time, and is the
// refusal when its grant is gone.
func TestCheckAgent(t *testing.T) {
	asked := 0
	revoked := false
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/apps/v1/workspaces/ws_1/agents/claude-code/token" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		asked++
		w.Header().Set("Content-Type", "application/json")
		if revoked {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"no_grant","message":"The grant is revoked."}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
	}))
	ctx := context.Background()
	if err := a.CheckAgent(ctx, "ws_1", "claude-code"); err != nil {
		t.Fatal(err)
	}
	revoked = true
	if err := a.CheckAgent(ctx, "ws_1", "claude-code"); !IsCode(err, "no_grant") {
		t.Errorf("%v, want no_grant", err)
	}
	if asked != 2 {
		t.Errorf("asked %d times, want 2", asked)
	}
}

// A publish URL is an event's: it has no topic.
func TestPublishURLHasNoTopic(t *testing.T) {
	b, _ := json.Marshal(PublishURL{ID: "pu_1", Event: "ci.finished", URL: "https://x"})
	if string(b) != `{"id":"pu_1","event":"ci.finished","url":"https://x"}` {
		t.Errorf("%s", b)
	}
}
