package gamekit

import (
	"encoding/json"
	"strings"
	"testing"
)

// Roles name no one: a label each, the house flag and model for house
// agents only, and nothing of a player's key or handle.
func TestAnonRoles(t *testing.T) {
	var a Anon
	drawer := Player{Workspace: "ws-1", Agent: "ag-1", Session: "s1", Handle: "dashing-pelican", House: "Haiku 5.5"}
	g1 := Player{Workspace: "ws-2", Agent: "ag-2", Handle: "sleek-otter"}
	g2 := Player{Workspace: "ws-3", Agent: "ag-3", Handle: "dashing-pelican-2", House: "GPT-6 Luna"}
	if got := a.As(drawer, "drawer"); got != "drawer" {
		t.Fatal(got)
	}
	if got := a.Numbered(g1, "guesser"); got != "guesser 1" {
		t.Fatal(got)
	}
	if got := a.Numbered(g2, "guesser"); got != "guesser 2" {
		t.Fatal(got)
	}
	// A player keeps its first role.
	if got := a.Numbered(g1, "guesser"); got != "guesser 1" {
		t.Fatal(got)
	}
	if got := a.As(drawer, "guesser 9"); got != "drawer" {
		t.Fatal(got)
	}
	if a.Role("sleek-otter") != "guesser 1" || a.Role("nobody") != "" {
		t.Fatal("Role")
	}
	ps := a.Players()
	want := []ArchivePlayer{{Role: "drawer", House: true, Model: "Haiku 5.5"}, {Role: "guesser 1"}, {Role: "guesser 2", House: true, Model: "GPT-6 Luna"}}
	if len(ps) != len(want) {
		t.Fatalf("%+v", ps)
	}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("player %d: %+v, want %+v", i, ps[i], want[i])
		}
	}
	b, _ := json.Marshal(ps)
	for _, leak := range []string{"ws-", "ag-", "s1", "pelican", "otter"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("players %s carry %q", b, leak)
		}
	}
	if string(must(json.Marshal((&Anon{}).Players()))) != "[]" {
		t.Error("no players should be []")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Handles in text become roles, in any case and with hyphens or spaces,
// as whole words only, the longest handle first.
func TestAnonScrub(t *testing.T) {
	var a Anon
	a.As(Player{Handle: "dashing-pelican"}, "drawer")
	a.Numbered(Player{Handle: "dashing-pelican-2"}, "guesser")
	a.Numbered(Player{Handle: "otter"}, "guesser")
	for in, want := range map[string]string{
		"dashing-pelican":                   "drawer",
		"Dashing Pelican 2":                 "guesser 1",
		"DASHING-PELICAN-2 is cheating":     "guesser 1 is cheating",
		"ask dashing  pelican":              "ask drawer",
		"otter otter":                       "guesser 2 guesser 2",
		"sea otters":                        "sea otters", // not a whole word
		"rotter":                            "rotter",
		"pelican":                           "pelican",
		"a fish":                            "a fish",
		"otter-dashing-pelican":             "guesser 2-drawer",
		"dashing-pelican-2 dashing-pelican": "guesser 1 drawer",
	} {
		if got := a.Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
	// A handle that a role contains doesn't loop.
	var b Anon
	b.Numbered(Player{Handle: "guesser"}, "guesser")
	if got := b.Scrub("guesser"); got != "guesser 1" {
		t.Errorf("got %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("héllo", 2) != "hé" || Truncate("abc", 5) != "abc" || Truncate("", 0) != "" {
		t.Error("Truncate")
	}
	if a, b := ArchiveID(), ArchiveID(); len(a) != 26 || a == b {
		t.Errorf("ArchiveID %q %q", a, b)
	}
}
