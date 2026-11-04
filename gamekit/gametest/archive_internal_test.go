package gametest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Archive looks for each seat's names as whole words in the record's
// decoded text: a handle inside a codeword isn't a leak (yak in kayak, which
// made TestHousePlayers fail now and then), a handle beside punctuation or
// after an escaped newline is, in any case.
func TestArchiveLooksForWholeWords(t *testing.T) {
	leaks := func(rec, name string) bool {
		var v any
		if err := json.Unmarshal([]byte(rec), &v); err != nil {
			t.Fatal(err)
		}
		return wholeWord(name).MatchString(strings.Join(jsonStrings(v, nil), "\n"))
	}
	for _, c := range []struct {
		rec, name string
		leak      bool
	}{
		{`{"codeword": "kayak"}`, "yak", false},
		{`{"clues": [{"text": "player 2 likes koalas"}]}`, "koala", false},
		{`{"clues": [{"text": "I'm less sure about @yak."}]}`, "yak", true},
		{`{"clues": [{"text": "first\nYAK knows"}]}`, "yak", true},
		{`{"yak": 1}`, "yak", true},
		{`{"players": [{"label": "player 1"}], "game": "g_9f2"}`, "g_9f2", true},
		{`{"clues": [{"text": "otter-2 again"}]}`, "otter-2", true},
		{`{"clues": [{"text": "otter-23 again"}]}`, "otter-2", false},
	} {
		if got := leaks(c.rec, c.name); got != c.leak {
			t.Errorf("%q in %s: leak %v, want %v", c.name, c.rec, got, c.leak)
		}
	}
}

// reporter is a testing.TB that keeps what Errorf reports.
type reporter struct {
	testing.TB
	reported []string
}

func (e *reporter) Errorf(format string, args ...any) {
	e.reported = append(e.reported, fmt.Sprintf(format, args...))
}

// Archive checks the record against the archived game's seats, not every
// seat in the store: a player waiting at another table may be called what
// the game's codeword is (Secret Agent picks a codeword no player in its
// game is called, but another table's handles are drawn from animals too),
// which made TestHousePlayers fail now and then ("walrus"). The game's own
// seats are still checked.
func TestArchiveChecksTheGamesSeats(t *testing.T) {
	store, _ := Store(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO games (id, phase, max_seats, state) VALUES ('played', 'next game', 3, '{}'), ('other', 'waiting', 3, '{}')`,
		`INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at) VALUES
			('played', 'w1/claude-code/s1', 'w1', 'claude-code', 's1', 'otter', 'otter', now()),
			('played', 'w2/codex/s2', 'w2', 'codex', 's2', 'heron', 'heron', now()),
			('other', 'w3/cursor/s3', 'w3', 'cursor', 's3', 'walrus', 'walrus', now())`,
		`INSERT INTO game_archives (played_on, record) VALUES (current_date, '{"codeword": "walrus", "players": [{"label": "player 1"}, {"label": "player 2"}]}')`,
	} {
		if _, err := store.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	e := &reporter{TB: t}
	Archive(e, store, "played")
	if len(e.reported) != 0 {
		t.Errorf("another table's seat counted against the record: %q", e.reported)
	}
	// A name of the game's own seat in the record is still a leak.
	if _, err := store.Pool.Exec(ctx, `UPDATE game_archives SET record = '{"codeword": "walrus", "clues": [{"text": "otter knows"}]}'`); err != nil {
		t.Fatal(err)
	}
	e = &reporter{TB: t}
	Archive(e, store, "played")
	if len(e.reported) != 1 || !strings.Contains(e.reported[0], `"otter"`) {
		t.Errorf("the game's own seat in the record: %q", e.reported)
	}
}
