package wakeshift_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// Replay and Once reach Wakeshift as every SDK sends them
// (subscribe-options.json): replay in seconds, once as true.
func TestSubscribeOptionsGolden(t *testing.T) {
	raw, err := os.ReadFile(sharedGolden("subscribe-options.json"))
	if err != nil {
		t.Fatal(err)
	}
	type optionCase struct {
		Name          string         `json:"name"`
		ReplaySeconds *float64       `json:"replay_seconds"`
		Once          bool           `json:"once"`
		Body          map[string]any `json:"body"`
		Error         wakeshift.Code `json:"error"`
		Says          string         `json:"says"`
	}
	var f struct {
		Cases        []optionCase `json:"cases"`
		ReplayBounds []optionCase `json:"replay_bounds"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.ReplayBounds) == 0 {
		t.Fatal("no replay_bounds")
	}
	w := newWrapped(t, wakeshift.AttachOptions{})
	type in struct {
		Table string `json:"table"`
		Case  string `json:"case"`
	}
	all := append(append([]optionCase{}, f.Cases...), f.ReplayBounds...)
	var errs = map[string]error{}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, a in) (*mcp.CallToolResult, any, error) {
		for _, c := range all {
			if c.Name != a.Case {
				continue
			}
			var opts []wakeshift.SubscribeOption
			if c.ReplaySeconds != nil {
				opts = append(opts, wakeshift.Replay(time.Duration(*c.ReplaySeconds*float64(time.Second))))
			}
			if c.Once {
				opts = append(opts, wakeshift.Once())
			}
			_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: a.Table}, opts...)
			errs[c.Name] = err
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		}
		return nil, nil, fmt.Errorf("no case %q", a.Case)
	}, wakeshift.Subscribes(w.shared))
	for _, c := range all {
		w.core.reset()
		if res := w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak", "case": c.Name}, nil); res.IsError || res.RPCError != nil {
			t.Fatalf("%s: %+v", c.Name, res)
		}
		if c.Error != "" {
			// Refused before sending, the same way in every SDK.
			if err := errs[c.Name]; !wakeshift.IsCode(err, c.Error) || !strings.Contains(err.Error(), c.Says) {
				t.Errorf("%s: %v, want %s saying %q", c.Name, err, c.Error, c.Says)
			}
			if len(w.core.bodies("/event-subscriptions")) != 0 {
				t.Errorf("%s: asked Wakeshift", c.Name)
			}
			continue
		}
		if err := errs[c.Name]; err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		body := w.core.bodies("/event-subscriptions")[0]
		got := map[string]any{}
		for _, k := range []string{"replay", "once"} {
			if v, ok := body[k]; ok {
				got[k] = v
			}
		}
		if !reflect.DeepEqual(got, c.Body) {
			t.Errorf("%s: %v, want %v", c.Name, got, c.Body)
		}
	}
}

// SigningSecret goes to Wakeshift once, and CreatePublishURL reads back
// its fingerprint (Emit doesn't take it: it's a PublishURLOption).
func TestPublishURLSigningSecret(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	ctx := context.Background()
	secret := "whsec_" + strings.Repeat("A", 43) + "="
	got, err := w.shared.CreatePublishURL(ctx, tableArgs{Table: "oak"}, wakeshift.SigningSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	if b := w.core.bodies("/events/publish-urls"); len(b) != 1 || b[0]["signing_secret"] != secret {
		t.Fatalf("the body: %v", b)
	}
	if got.Signing == nil || got.Signing.Fingerprint != "abc123def456" || !got.Signing.SetAt.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("signing: %+v", got.Signing)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), secret) {
		t.Fatal("the secret came back")
	}
	w.core.reset()
	if plain, err := w.shared.CreatePublishURL(ctx, tableArgs{Table: "oak"}); err != nil || plain.Signing != nil || w.core.bodies("/events/publish-urls")[0]["signing_secret"] != nil {
		t.Fatalf("without a secret: %+v %v", plain, err)
	}
	w.core.reset()
	if _, err := w.shared.CreatePublishURL(ctx, tableArgs{Table: "oak"}, wakeshift.SigningSecret(" ")); !wakeshift.IsCode(err, "bad_signing_secret") {
		t.Fatalf("an empty secret: %v", err)
	}
	if n := len(w.core.paths()); n != 0 {
		t.Fatalf("refused calls reached Wakeshift: %v", w.core.paths())
	}
}
