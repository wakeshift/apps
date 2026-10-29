package gamekit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/internal/pgtest"
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

// Handles in text become roles, in any case and with hyphens, underscores
// or spaces, as whole words only, the longest handle first: the cases in
// testdata/scrub.json, which Agent Hold'em's scrubber runs too.
func TestAnonScrub(t *testing.T) {
	b, err := os.ReadFile("testdata/scrub.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name    string `json:"name"`
			Players []struct {
				Name string `json:"name"`
				Role string `json:"role"`
			} `json:"players"`
			Text string `json:"text"`
			Want string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		var a Anon
		for _, p := range c.Players {
			a.As(Player{Handle: p.Name}, p.Role)
		}
		if got := a.Scrub(c.Text); got != c.Want {
			t.Errorf("%s: Scrub(%q) = %q, want %q", c.Name, c.Text, got, c.Want)
		}
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

// A room's archive ID is random, the same for every record while the room
// is open, and its link to the game goes when the game ends, when it's
// stopped, or once it's gone unused for archiveRoomIdle.
func TestArchiveRoom(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	now := time.Now()
	s := &Server{Name: "g", Store: st, Now: func() time.Time { return now }, NewState: func() State { return &Game{} }}
	for _, id := range []string{"g1", "g2", "g3"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ($1, 'playing', 3, $2)`, id, fmt.Sprintf(`{"id":%q,"phase":"playing"}`, id)); err != nil {
			t.Fatal(err)
		}
	}
	room := func(game string) string {
		t.Helper()
		var id string
		if _, err := s.Update(ctx, game, nil, func(tx *Tx, g State) error {
			var err error
			id, err = tx.ArchiveRoom(game)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	linked := func(game string) bool {
		var n int
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM archive_rooms WHERE game_id = $1`, game).Scan(&n)
		return n == 1
	}
	a := room("g1")
	if len(a) != 26 || a == "g1" || room("g1") != a || room("g2") == a {
		t.Fatalf("archive IDs %q %q %q", a, room("g1"), room("g2"))
	}
	room("g3")
	// The game ends: its link goes.
	if _, err := s.Update(ctx, "g1", nil, func(tx *Tx, g State) error { g.(*Game).End(time.Minute); return nil }); err != nil {
		t.Fatal(err)
	}
	if linked("g1") {
		t.Error("an ended game is still linked to its archive ID")
	}
	// Stopped: likewise.
	if _, err := s.Update(ctx, "g2", nil, func(tx *Tx, g State) error { return s.stop(tx, "g2", "test") }); err != nil {
		t.Fatal(err)
	}
	if linked("g2") {
		t.Error("a stopped game is still linked to its archive ID")
	}
	// Unused for archiveRoomIdle: it goes, and the room gets a new one.
	now = now.Add(archiveRoomIdle + time.Minute)
	s.forgotAt.Store(0)
	if err := s.forget(ctx, now); err != nil {
		t.Fatal(err)
	}
	if linked("g3") {
		t.Error("an idle link was kept")
	}
	// Deleting the game deletes its link.
	b := room("g3")
	if _, err := st.Pool.Exec(ctx, `DELETE FROM games WHERE id = 'g3'`); err != nil {
		t.Fatal(err)
	}
	if b == "" || linked("g3") {
		t.Error("a deleted game's link was kept")
	}
}
