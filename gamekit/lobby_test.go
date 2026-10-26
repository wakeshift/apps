package gamekit

import (
	"context"
	"testing"

	"github.com/wakeshift/apps/internal/pgtest"
)

// The lobby lists open games as waiting; with LobbyPhase, only those in
// that phase (a game that stays open while it plays counts as playing).
func TestLobbyPhase(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	for _, g := range []struct {
		id, phase string
		open      bool
	}{{"waits", "waiting", true}, {"plays", "playing", true}, {"closed", "playing", false}} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, open, max_seats, state) VALUES ($1, $2, $3, 8, '{}')`, g.id, g.phase, g.open); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at) VALUES ($1, 'w/a/s', 'w', 'a', 's', 'x', 'x', now(), now())`, g.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		phase   string
		waiting []string
		playing int
	}{{"", []string{"waits", "plays"}, 1}, {"waiting", []string{"waits"}, 2}} {
		s := &Server{Store: st, LobbyPhase: tc.phase}
		l, err := s.Lobby(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, g := range l.Waiting {
			ids = append(ids, g.ID)
		}
		if len(ids) != len(tc.waiting) || (len(ids) > 0 && ids[0] != tc.waiting[0]) || l.Playing.Games != tc.playing || l.Playing.Real != tc.playing {
			t.Errorf("LobbyPhase %q: waiting %v, playing %+v; want %v and %d", tc.phase, ids, l.Playing, tc.waiting, tc.playing)
		}
	}
}
