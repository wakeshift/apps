package appkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The TypeScript and Python SDKs' wrapped tools and emits read the same
// files (sdk/typescript/test), so the three can't drift: wrapped-block.json
// is what a reply looks like once Wakeshift's block is added, and
// emit-recipients.json how an emit's recipients go on the wire and how its
// answer is read (sharedGoldenDir: in the published Go SDK, they're in
// testdata/golden).

func readSharedFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// plainJSON is v as plain JSON values, without a false isError.
func plainJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out["isError"] == false {
		delete(out, "isError")
	}
	return out
}

func TestWrappedBlockGolden(t *testing.T) {
	type addition struct {
		Text    string         `json:"text"`
		Trailer string         `json:"trailer"`
		Meta    map[string]any `json:"meta"`
	}
	var f struct {
		Addition addition `json:"addition"`
		Cases    []struct {
			Name       string          `json:"name"`
			Addition   *addition       `json:"addition"`
			WakeOutput bool            `json:"wake_output"`
			Result     json.RawMessage `json:"result"`
			Want       json.RawMessage `json:"want"`
		} `json:"cases"`
	}
	readSharedFixture(t, "wrapped-block.json", &f)
	for _, c := range f.Cases {
		var s string
		if json.Unmarshal(c.Result, &s) == nil {
			continue // a string result: Go's handlers return a CallToolResult
		}
		add := f.Addition
		if c.Addition != nil {
			add = *c.Addition
		}
		var res mcp.CallToolResult
		if err := json.Unmarshal(c.Result, &res); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		// What addBlock does with /calls/addition's answer.
		text := add.Text
		mergeBlock(&res, text, add.Trailer, add.Meta)
		if text != "" && c.WakeOutput {
			setWakeshift(&res, text)
		}
		var want any
		json.Unmarshal(c.Want, &want)
		if got := plainJSON(t, &res); !reflect.DeepEqual(got, want) {
			gb, _ := json.Marshal(got)
			t.Errorf("%s:\n got %s\nwant %s", c.Name, gb, c.Want)
		}
	}
}

// recipientJSON is a Recipient in an emit's to forms.
func recipientJSON(r Recipient) any {
	switch {
	case r.Subscription != "":
		return map[string]any{"user": r.User, "subscription": r.Subscription}
	case r.Native:
		return map[string]any{"native": r.User}
	}
	return r.User
}

// recipientsOfJSON reads to's forms as Recipients.
func recipientsOfJSON(t *testing.T, raw []json.RawMessage) []Recipient {
	t.Helper()
	out := []Recipient{}
	for _, r := range raw {
		var u string
		if json.Unmarshal(r, &u) == nil {
			out = append(out, Recipient{User: u})
			continue
		}
		var m struct{ User, Subscription, Native *string }
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case m.Native != nil:
			out = append(out, Recipient{User: *m.Native, Native: true})
		default:
			out = append(out, Recipient{User: *m.User, Subscription: *m.Subscription})
		}
	}
	return out
}

func TestEmitRecipientsGolden(t *testing.T) {
	var f struct {
		Cases []struct {
			Name string            `json:"name"`
			To   []json.RawMessage `json:"to"`
			Wire json.RawMessage   `json:"wire"`
		} `json:"cases"`
		Responses []struct {
			Name     string          `json:"name"`
			Response json.RawMessage `json:"response"`
			Emitted  json.RawMessage `json:"emitted"`
		} `json:"responses"`
		Refused []struct {
			Name string            `json:"name"`
			To   []json.RawMessage `json:"to"`
		} `json:"refused"`
		RefusedCode string `json:"refused_code"`
		OneUser     struct {
			To          string   `json:"to"`
			Wire        string   `json:"wire"`
			Refused     []string `json:"refused"`
			RefusedCode string   `json:"refused_code"`
		} `json:"one_user"`
		StatusOf struct {
			Subscriptions []struct{ User, Subscription string }   `json:"subscriptions"`
			Wire          json.RawMessage                         `json:"wire"`
			Refused       [][]struct{ User, Subscription string } `json:"refused"`
			RefusedCode   string                                  `json:"refused_code"`
		} `json:"status_of"`
	}
	readSharedFixture(t, "emit-recipients.json", &f)
	// A core that records each request's body by path.
	var mu sync.Mutex
	bodies := map[string][]map[string]any{}
	last := func(path string) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies[path]) == 0 {
			return nil
		}
		return bodies[path][len(bodies[path])-1]
	}
	app, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		mu.Lock()
		bodies[path] = append(bodies[path], body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case path == "/events/status" && body["subscriptions"] != nil:
			io.WriteString(w, `{"subscriptions":[]}`)
		case path == "/events/status":
			io.WriteString(w, `{"subscriptions":0,"connected":0}`)
		default:
			io.WriteString(w, `{"matched":0,"id":"p1","url":"https://x"}`)
		}
	}))
	ev, err := DefineEvent(app, "t.turn", EventDef[internalArgs, internalArgs]{Description: "E.", Personal: true, Text: func(internalArgs, internalArgs) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	optsOf := func(rs []Recipient) []EmitOption {
		var opts []EmitOption
		for _, r := range rs {
			switch {
			case r.Native:
				opts = append(opts, ToNative(r.User))
			case r.Subscription != "":
				opts = append(opts, ToSubscription(r.User, r.Subscription))
			default:
				opts = append(opts, To(r.User))
			}
		}
		return opts
	}
	for _, c := range f.Cases {
		if _, err := ev.Emit(ctx, internalArgs{Table: "oak"}, internalArgs{}, optsOf(recipientsOfJSON(t, c.To))...); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		got, _ := json.Marshal(last("/events/emit")["to"])
		var g, w any
		json.Unmarshal(got, &g)
		json.Unmarshal(c.Wire, &w)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: to %s, want %s", c.Name, got, c.Wire)
		}
	}
	// A personal event's status and publish URL name one user, trimmed the
	// same way.
	if _, err := ev.Status(ctx, internalArgs{Table: "oak"}, To(f.OneUser.To)); err != nil {
		t.Fatal(err)
	}
	if _, err := ev.PublishURL(ctx, internalArgs{Table: "oak"}, To(f.OneUser.To)); err != nil {
		t.Fatal(err)
	}
	if s, p := last("/events/status")["to"], last("/events/publish-urls")["to"]; s != f.OneUser.Wire || p != f.OneUser.Wire {
		t.Errorf("one user: status to %v, publish URL to %v, want %q", s, p, f.OneUser.Wire)
	}
	for _, to := range f.OneUser.Refused {
		if _, err := ev.Status(ctx, internalArgs{Table: "oak"}, To(to)); !IsCode(err, f.OneUser.RefusedCode) {
			t.Errorf("status to %q: %v", to, err)
		}
		if _, err := ev.PublishURL(ctx, internalArgs{Table: "oak"}, To(to)); !IsCode(err, f.OneUser.RefusedCode) {
			t.Errorf("publish URL to %q: %v", to, err)
		}
	}
	var subs []Recipient
	for _, s := range f.StatusOf.Subscriptions {
		subs = append(subs, Recipient{User: s.User, Subscription: s.Subscription})
	}
	if _, err := ev.StatusOf(ctx, subs...); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(last("/events/status")["subscriptions"])
	var g, w any
	json.Unmarshal(got, &g)
	json.Unmarshal(f.StatusOf.Wire, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("status of: %s, want %s", got, f.StatusOf.Wire)
	}
	for _, refused := range f.StatusOf.Refused {
		var subs []Recipient
		for _, s := range refused {
			subs = append(subs, Recipient{User: s.User, Subscription: s.Subscription})
		}
		if _, err := ev.StatusOf(ctx, subs...); !IsCode(err, f.StatusOf.RefusedCode) {
			t.Errorf("status of %+v: %v", refused, err)
		}
	}
	for _, c := range f.Responses {
		var out struct {
			Emitted
			Unmatched []json.RawMessage `json:"unmatched"`
			Ended     []json.RawMessage `json:"ended"`
		}
		if err := json.Unmarshal(c.Response, &out); err != nil {
			t.Fatal(err)
		}
		res := out.Emitted
		res.Unmatched, res.Ended = recipientsOf(out.Unmatched), recipientsOf(out.Ended)
		got := map[string]any{"matched": res.Matched, "duplicate": res.Duplicate, "unmatched": []any{}, "ended": []any{}}
		for _, r := range res.Unmatched {
			got["unmatched"] = append(got["unmatched"].([]any), recipientJSON(r))
		}
		for _, r := range res.Ended {
			got["ended"] = append(got["ended"].([]any), recipientJSON(r))
		}
		var want any
		json.Unmarshal(c.Emitted, &want)
		if !reflect.DeepEqual(plainJSON(t, got), want) {
			gb, _ := json.Marshal(got)
			t.Errorf("%s: emitted %s, want %s", c.Name, gb, c.Emitted)
		}
	}
	mu.Lock()
	emits := len(bodies["/events/emit"])
	mu.Unlock()
	for _, c := range f.Refused {
		if _, err := ev.Emit(ctx, internalArgs{Table: "oak"}, internalArgs{}, optsOf(recipientsOfJSON(t, c.To))...); !IsCode(err, f.RefusedCode) {
			t.Errorf("%s: %v, want %s", c.Name, err, f.RefusedCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies["/events/emit"]) != emits {
		t.Errorf("a refused emit asked Wakeshift")
	}
}
