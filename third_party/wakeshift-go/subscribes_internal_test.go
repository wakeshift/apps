package wakeshift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The shared fixture: the sentence tools/list adds to a tool that
// subscribes, the same in every SDK (subscribes.json compact_cases; the
// bench kept the wording as it was, and it's added always).
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
		} `json:"compact_cases"`
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

// The compact strings live in one place, compactStrings, and match the
// shared fixture's (subscribing-tools-list.json compact_strings), and
// Wakeshift's arguments as tools/list adds them match wake-args.json
// compact.
func TestCompactStringsGolden(t *testing.T) {
	if _, err := os.Stat(goldenDir); err != nil {
		t.Skip("no shared golden files here")
	}
	var f struct {
		Compact map[string]any `json:"compact_strings"`
	}
	readSharedFixture(t, "subscribing-tools-list.json", &f)
	want := map[string]any{
		"session_key": compactStrings.SessionKey, "harness": compactStrings.Harness, "conversation_id": compactStrings.ConversationID,
		"arguments_argument": compactStrings.ArgumentsArgument, "event_argument": compactStrings.EventArgument, "id_argument": compactStrings.IDArgument,
		"subscribe_description": compactStrings.SubscribeDescription, "unsubscribe_description": compactStrings.UnsubscribeDescription,
		"list_description": compactStrings.ListDescription,
		"notifies":         compactStrings.Notifies, "notifies_event": compactStrings.NotifiesEvent,
		"notifies_events": compactStrings.NotifiesEvents, "notifies_events_separator": compactStrings.NotifiesEventsSeparator,
	}
	got := map[string]any{}
	for k, v := range f.Compact {
		if k[0] != '_' {
			got[k] = v
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("compact strings\n    SDK %v\nfixture %v", want, got)
	}

	var w struct {
		Compact struct {
			Subscribing       map[string]any `json:"subscribing"`
			SubscribesNothing map[string]any `json:"subscribes_nothing"`
		} `json:"compact"`
	}
	readSharedFixture(t, "wake-args.json", &w)
	for all, want := range map[bool]map[string]any{true: w.Compact.Subscribing, false: w.Compact.SubscribesNothing} {
		if g := canonical(t, map[string]any{"properties": wakeArgProperties(all)}); !reflect.DeepEqual(g, canonical(t, want)) {
			t.Errorf("wake arguments (all %v)\n got %v\nwant %v", all, g, want)
		}
	}
}
