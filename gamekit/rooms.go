package gamekit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Rooms: a game's group in Wakeshift, the players seated in it, and what's
// said there.

// Player is a seat in a room: a conversation (one of an agent's sessions),
// or an agent as a whole without one.
type Player struct {
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
	Session   string `json:"session,omitempty"`
	Handle    string `json:"handle"` // in the room
}

// Key identifies the player across games: workspace/agent/session.
func (p Player) Key() string { return PlayerKey(p.Workspace, p.Agent, p.Session) }

// PlayerKey is a player's key.
func PlayerKey(workspace, agent, session string) string {
	return workspace + "/" + agent + "/" + session
}

// Entry is something said in a room: by the moderator (From "") or a
// player.
type Entry struct {
	At       time.Time `json:"at"`
	From     string    `json:"from,omitempty"`
	Text     string    `json:"text"`
	Subject  string    `json:"-"`
	Mentions []string  `json:"-"`
}

// names are what the kit calls players in a room, rather than their agents'
// IDs ("codex-2"): friendlier, and they don't give away which harness is
// playing.
var names = []string{
	"raven", "otter", "falcon", "badger", "heron", "lynx", "marten", "osprey", "puffin", "wren", "sparrow", "magpie",
	"coyote", "bison", "ibex", "jackal", "newt", "panther", "quail", "stoat", "tern", "weasel", "finch", "gecko",
	"hare", "kestrel", "lemur", "mink", "ocelot", "pika", "robin", "shrew", "toad", "vole", "beaver", "cougar",
	"dingo", "egret", "ferret", "gopher", "hornet", "impala", "jaguar", "koala", "llama", "moose", "narwhal", "oriole",
	"pelican", "quokka", "salmon", "tapir", "urchin", "walrus", "yak", "zebra", "condor", "dormouse", "gannet", "marmot",
}

// pickName is a name not yet used in a room.
func pickName(used []string) string {
	free := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(used, n) })
	if len(free) == 0 {
		free = names // Wakeshift adds -2, -3… to keep handles unique
	}
	return free[RandInt(len(free))]
}

// room is a game's group in Wakeshift.
func (s *Server) room(id string) string { return s.Prefix + id }

// Join seats the calling conversation in the next game with a free seat (a
// new one if there's none) and returns it: matchmaking reserves the seat,
// the conversation joins the game's room in Wakeshift under the name the
// room gives it (it's woken by everything said there), and seat adds the
// player to the game, under its lock. A conversation already playing gets
// its game back (and a zero Player). If the game started while the
// conversation was joining its room, it tries the next.
func (s *Server) Join(ctx context.Context, ag *appkit.Agent, session string, seat func(tx *Tx, g State, p Player) error) (State, Player, error) {
	key := PlayerKey(ag.Workspace, ag.ID, session)
	for range 3 {
		r, err := s.reserve(ctx, ag.Workspace, ag.ID, session)
		if err != nil {
			return nil, Player{}, err
		}
		if r.current {
			// Back in the room if its subscription ended while it was
			// seated (it's been away since).
			if err := s.reseat(ctx, ag, r.game, session, r.handle); err != nil {
				return nil, Player{}, err
			}
			g, err := s.Load(ctx, r.game)
			return g, Player{}, err
		}
		p, err := s.seat(ctx, ag, r, session)
		if err != nil {
			s.unreserve(ctx, r.game, key)
			return nil, Player{}, err
		}
		taken := true
		g, err := s.Update(ctx, r.game, nil, func(tx *Tx, g State) error {
			if !g.base().Open {
				taken = false
				_, err := tx.Exec(tx.Ctx, `DELETE FROM seats WHERE game_id = $1 AND player = $2`, r.game, key)
				return err
			}
			tag, err := tx.Exec(tx.Ctx, `UPDATE seats SET handle = $3, seated_at = $4 WHERE game_id = $1 AND player = $2`,
				r.game, key, p.Handle, tx.Now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// The reservation was swept as abandoned while this was
				// joining the room: no seat here, so the next game.
				taken = false
				return nil
			}
			return seat(tx, g, p)
		})
		if err != nil {
			s.unreserve(ctx, r.game, key)
			s.leaveRoom(ctx, r.game, p.Handle)
			return nil, Player{}, err
		}
		if taken {
			return g, p, nil
		}
		s.leaveRoom(ctx, r.game, p.Handle) // it started without this player: the next game
	}
	return nil, Player{}, ErrBusy
}

// seat adds the conversation to a reserved seat's room in Wakeshift.
func (s *Server) seat(ctx context.Context, ag *appkit.Agent, r reservation, session string) (Player, error) {
	typ := s.room(r.game)
	if !r.roomReady {
		// The game's first player makes its room (and any who reserved a
		// seat before that was done: it's idempotent).
		if _, _, err := s.Kit.CreateGroup(ctx, typ, s.Title+" game "+r.game, nil); err != nil {
			return Player{}, err
		}
		if _, err := s.Store.Pool.Exec(ctx, `UPDATE games SET room_ready = true WHERE id = $1`, r.game); err != nil {
			return Player{}, err
		}
	}
	if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: typ, Wake: "all", Reason: "Playing " + s.Title + " game " + r.game, Session: session,
		Handle: r.name}); err != nil {
		if appkit.IsCode(err, "outside_scope") {
			return Player{}, fmt.Errorf("this agent's consent for %s doesn't cover games; sign in to %s again", s.Title, s.Title)
		}
		return Player{}, err
	}
	g, err := s.Kit.Group(ctx, typ)
	if err != nil {
		return Player{}, err
	}
	i := slices.IndexFunc(g.Members, func(m appkit.GroupMember) bool {
		return m.Workspace == ag.Workspace && m.Agent == ag.ID && m.Session == session
	})
	if i < 0 {
		return Player{}, errors.New("joining the game didn't take; try again")
	}
	return Player{Workspace: ag.Workspace, Agent: ag.ID, Session: session, Handle: g.Members[i].Handle}, nil
}

// Human is a duration as players read it: "2 minutes", "1 minute 30
// seconds", "1 hour" (rounded to the second).
func Human(d time.Duration) string {
	d = d.Round(time.Second)
	var parts []string
	for _, u := range []struct {
		d    time.Duration
		name string
	}{{time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if n := int(d / u.d); n > 0 {
			d -= time.Duration(n) * u.d
			s := fmt.Sprintf("%d %s", n, u.name)
			if n != 1 {
				s += "s"
			}
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "0 seconds"
	}
	return strings.Join(parts, " ")
}

// subjectMax is the longest subject Wakeshift takes, in bytes.
const subjectMax = 200

// truncate cuts s to at most n bytes, on a character boundary, ending with
// "…" when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	cut := n - len(more)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + more
}

// leaveRoom takes a player out of a game's room it didn't get a seat in.
func (s *Server) leaveRoom(ctx context.Context, game, handle string) {
	if handle == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.Kit.RemoveMember(ctx, s.room(game), handle); err != nil {
		slog.Warn(s.Name+": leaving a room", "game", game, "err", err)
	}
}

// Away lists the game's players that nothing is delivering to now: ones
// whose agent dropped off, and seated ones no longer in the room at all
// (their subscription ended: unsubscribed, expired or revoked). A game
// needn't wait for them. On an error it lists none, so the game waits as
// before.
func (s *Server) Away(ctx context.Context, game string) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, _ := s.Store.Pool.Query(ctx, `SELECT handle FROM seats WHERE game_id = $1 AND handle IS NOT NULL`, game)
	seated, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		slog.Warn(s.Name+": reading a game's seats", "game", game, "err", err)
		return nil
	}
	g, err := s.Kit.Group(ctx, s.room(game))
	if err != nil {
		slog.Warn(s.Name+": checking who's connected", "game", game, "err", err)
		return nil
	}
	away := map[string]bool{}
	for _, h := range seated {
		away[h] = true
	}
	for _, m := range g.Members {
		delete(away, m.Handle)
		if m.Connected != nil && !*m.Connected {
			away[m.Handle] = true
		}
	}
	return away
}

// reseat puts a seated player back in its game's room after its
// subscription ended (it was away for good), under the handle its seat
// has. A player still in the room is left as it is.
func (s *Server) reseat(ctx context.Context, ag *appkit.Agent, game, session, handle string) error {
	typ := s.room(game)
	g, err := s.Kit.Group(ctx, typ)
	if err != nil {
		return err
	}
	// The player itself, under its seat's handle: someone else with that
	// handle doesn't count.
	isPlayer := func(m appkit.GroupMember) bool {
		return m.Handle == handle && m.Workspace == ag.Workspace && m.Agent == ag.ID && m.Session == session
	}
	if slices.ContainsFunc(g.Members, isPlayer) {
		return nil
	}
	if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: typ, Wake: "all", Reason: "Playing " + s.Title + " game " + game, Session: session,
		Handle: handle}); err != nil {
		return err
	}
	if g, err = s.Kit.Group(ctx, typ); err != nil {
		return err
	}
	if !slices.ContainsFunc(g.Members, isPlayer) {
		return errors.New("rejoining the game's room didn't take; try again")
	}
	return nil
}

// Say logs what a player says and posts it to the room, as its agent. It's
// logged first, so what players see is in the log: if the post fails, the
// entry is taken back out.
func (s *Server) Say(ctx context.Context, ag *appkit.Agent, game string, p Player, text string) error {
	subject := truncate(text, subjectMax)
	now := s.now()
	var seq int64
	if err := s.Store.Pool.QueryRow(ctx, `INSERT INTO room_log (game_id, at, from_handle, subject, text, posted_at) VALUES ($1, $2, $3, $4, $5, $2) RETURNING seq`,
		game, now, p.Handle, subject, text).Scan(&seq); err != nil {
		return err
	}
	if _, err := ag.Publish(ctx, appkit.Post{Type: s.room(game), Subject: subject, Body: text, Data: map[string]any{"text": text}, Session: p.Session}); err != nil {
		if _, derr := s.Store.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM room_log WHERE game_id = $1 AND seq = $2`, game, seq); derr != nil {
			slog.Warn(s.Name+": taking back an unposted message", "game", game, "err", derr)
		}
		return err
	}
	return nil
}
