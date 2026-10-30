package gamekit

import (
	"context"
	"strings"
	"testing"
	"time"
)

const endPath = "/api/apps/v1/event-subscriptions/end"

// remove takes player (by handle, in workspace w-<handle>) out of game in
// a move of its own, with text as its farewell, and sends what it left.
func remove(t *testing.T, s *Server, game, handle, text string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ok, err := tx.Remove(game, "w-"+handle+"/a/s", "You were taken out", text)
	if err != nil || !ok {
		t.Fatalf("remove %s: %v %v", handle, ok, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	s.deliver(ctx, game)
}

// calls are the paths of the core calls made, emits and ends only.
func (f *recCore) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		switch c.Path {
		case emitPath:
			out = append(out, "emit "+js(c.Body["to"]))
		case endPath:
			out = append(out, "end "+c.Body["user"].(string)+" "+c.Body["id"].(string))
		}
	}
	return out
}

// A removed player is told why, in an update to it alone that mentions it,
// before its seat's subscription is ended: Wakeshift's notice that the
// subscription ended comes after the game's own explanation. The room's
// log leaves the farewell out. A native seat is told, and keeps its
// subscription; a farewell that can't be sent holds the end back until it
// is; and with no text, the subscription just ends.
func TestRemoveTellsThePlayerFirst(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	putGame(t, s, "g1", "raven:sub-r", "otter:sub-o", "finch:native", "lynx:sub-l")

	remove(t, s, "g1", "otter", "Away for 2 rounds in a row. Call `join` to play again.")
	seq := core.sequence()
	if len(seq) != 2 || seq[0] != `emit [{"subscription":"sub-o","user":"w-otter/a"}]` || seq[1] != "end w-otter/a sub-o" {
		t.Fatalf("calls %v, want the farewell to otter alone, then its subscription's end", seq)
	}
	e := core.bodies(emitPath)[0]
	if p, _ := e["payload"].(map[string]any); p["text"] != "Away for 2 rounds in a row. Call `join` to play again." || p["mentioned"] != true || p["from"] != "" {
		t.Fatalf("the farewell: %v", e)
	}
	if log, _ := s.Log(ctx, "g1"); len(log) != 0 {
		t.Fatalf("the room's log shows the farewell: %+v", log)
	}

	// A native seat: told, through its native subscription; nothing ended.
	core.reset()
	remove(t, s, "g1", "finch", "You were taken out.")
	if seq := core.sequence(); len(seq) != 1 || seq[0] != `emit [{"native":"w-finch/a"}]` {
		t.Fatalf("native: %v", seq)
	}

	// Wakeshift is down: the end waits for the farewell.
	core.reset()
	down := true
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		if down {
			return 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}}
		}
		return 200, map[string]any{"matched": 1}
	})
	remove(t, s, "g1", "lynx", "You were taken out.")
	if ended := core.bodies(endPath); len(ended) != 0 {
		t.Fatalf("ended before the farewell went: %v", ended)
	}
	down = false
	core.reset()
	s.deliver(ctx, "g1")
	if seq := core.sequence(); len(seq) != 2 || !strings.HasPrefix(seq[0], "emit ") || seq[1] != "end w-lynx/a sub-l" {
		t.Fatalf("after Wakeshift is back: %v", seq)
	}

	// No text (it left at its own request): nothing sent, the end alone.
	core.reset()
	remove(t, s, "g1", "raven", "")
	if seq := core.sequence(); len(seq) != 1 || seq[0] != "end w-raven/a sub-r" {
		t.Fatalf("a leaver: %v", seq)
	}
}

// A farewell given up (8 failed tries) ends the subscription anyway.
func TestRemoveGivenUpStillEnds(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	putGame(t, s, "g1", "otter:sub-o")
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		return 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}}
	})
	remove(t, s, "g1", "otter", "You were taken out.")
	s.Store.Pool.Exec(ctx, `UPDATE room_log SET tries = 7 WHERE game_id = 'g1'`)
	s.deliver(ctx, "g1")
	if seq := core.sequence(); len(seq) != 3 || seq[2] != "end w-otter/a sub-o" {
		t.Fatalf("calls %v", seq)
	}
}

// A game that's over closes its room once its last word is sent: while
// Wakeshift won't take it, the players' subscriptions aren't ended (they'd
// hear only that the subscription ended), up to lastWordWait.
func TestCloseWaitsForTheLastWord(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	now := time.Now()
	s.Now = func() time.Time { return now }
	putGame(t, s, "g1", "otter:sub-o")
	if _, err := s.Store.Pool.Exec(ctx, `UPDATE games SET ended_at = $1, deadline_at = $1 WHERE id = 'g1'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO room_log (game_id, at, subject, text) VALUES ('g1', $1, 'Game over', 'The villagers win.')`, now); err != nil {
		t.Fatal(err)
	}
	down := true
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		if down {
			return 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}}
		}
		return 200, map[string]any{"matched": 1}
	})
	if _, err := s.advance(ctx, "g1", nil); err != nil {
		t.Fatal(err)
	}
	if ended := core.bodies(endPath); len(ended) != 0 {
		t.Fatalf("ended before the last word went: %v", ended)
	}
	var closed bool
	s.Store.Pool.QueryRow(ctx, `SELECT room_closed FROM games WHERE id = 'g1'`).Scan(&closed)
	if closed {
		t.Fatal("the room closed with its last word unsent")
	}
	down = false
	core.reset()
	now = now.Add(closeRetry)
	if _, err := s.advance(ctx, "g1", nil); err != nil {
		t.Fatal(err)
	}
	if seq := core.sequence(); len(seq) != 2 || !strings.HasPrefix(seq[0], "emit ") || seq[1] != "end w-otter/a sub-o" {
		t.Fatalf("calls %v, want the last word, then the end", seq)
	}

	// One whose last word can't go for lastWordWait closes anyway.
	putGame(t, s, "g2", "lynx:sub-l")
	s.Store.Pool.Exec(ctx, `UPDATE games SET ended_at = $1, deadline_at = $1 WHERE id = 'g2'`, now)
	s.Store.Pool.Exec(ctx, `INSERT INTO room_log (game_id, at, subject, text) VALUES ('g2', $1, 'Game over', 'Over.')`, now)
	down = true
	s.advance(ctx, "g2", nil)
	now = now.Add(lastWordWait)
	core.reset()
	if _, err := s.advance(ctx, "g2", nil); err != nil {
		t.Fatal(err)
	}
	if ended := core.bodies(endPath); len(ended) != 1 {
		t.Fatalf("not closed after lastWordWait: %v", core.sequence())
	}
}
