package gamekit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Rooms: the players seated in a game, each seat's conversation's
// game.update subscription, and what's said there (room_log, which is the
// outbox of what each seated player is sent: outbox.go).

// Player is a seat in a room: a conversation (one of an agent's sessions),
// or, for a client that subscribes to events itself, its conversation as it
// names it ("native:<conversation>": a house agent's is "native:").
type Player struct {
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
	Session   string `json:"session,omitempty"`
	Handle    string `json:"handle"` // in the room
	// House is a house agent's model as shown ("Haiku 4.5"), from its
	// verified claim (house.go); empty for everyone else.
	House string `json:"house,omitempty"`
}

// Key identifies the player across games: workspace/agent/session.
func (p Player) Key() string { return PlayerKey(p.Workspace, p.Agent, p.Session) }

// PlayerKey is a player's key.
func PlayerKey(workspace, agent, session string) string {
	return workspace + "/" + agent + "/" + session
}

// nativeSession is a native seat's session: the client's conversation, as
// ChatGPT names it in _meta["openai/session"] (two of an agent's ChatGPT
// conversations are two seats), "" for a client that names none (house
// agents): keyed as Agent Hold'em keys its native seats.
func nativeSession(conversation string) string { return "native:" + conversation }

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
		free = names // the handle gets -2, -3… to keep it unique (freeHandle)
	}
	return free[RandInt(len(free))]
}

// freeHandle is base, or base-2, base-3…, whichever no one seated in the
// game has. Called under the game's lock, which every seating takes.
func freeHandle(tx *Tx, game, base string) (string, error) {
	rows, _ := tx.Query(tx.Ctx, `SELECT handle FROM seats WHERE game_id = $1 AND handle IS NOT NULL`, game)
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	h := base
	for i := 2; slices.Contains(taken, h); i++ {
		h = base + "-" + strconv.Itoa(i)
	}
	return h, nil
}

// joining is the conversation a join seats: its agent and session, whether
// it subscribes to events itself, and how to subscribe it.
type joining struct {
	workspace, agent, session string
	native                    bool
	// subscribe subscribes the conversation to game.update (or, for a
	// native caller, tells it to subscribe itself: an empty ID).
	subscribe func(ctx context.Context) (appkit.Subscription, error)
}

// joiningOf is the calling conversation of a join: a native client's seat
// is keyed by its conversation (nativeSession), unless it passes a
// session_key; any other conversation's session is the one it passes, or
// the one Wakeshift finds or starts for it (a Codex thread's own).
func (s *Server) joiningOf(ctx context.Context, call *appkit.ToolCall) (joining, error) {
	ag := call.Agent
	if ag == nil {
		return joining{}, errors.New("not signed in")
	}
	j := joining{workspace: ag.Workspace, agent: ag.ID, native: call.Native, session: call.Session}
	if j.session == "" {
		if call.Native {
			j.session = nativeSession(call.NativeConversation)
		} else {
			session, err := call.ResolveSession(ctx)
			if err != nil {
				return joining{}, friendlySession(err)
			}
			j.session = session
		}
	}
	j.subscribe = func(ctx context.Context) (appkit.Subscription, error) {
		sub, err := s.events().Subscribe(ctx, call, UpdateArgs{}, appkit.Reason("Playing "+s.Title))
		if appkit.IsCode(err, "outside_scope") {
			return sub, fmt.Errorf("this agent's consent for %s doesn't cover games; sign in to %s again", s.Title, s.Title)
		}
		return sub, err
	}
	return j, nil
}

// friendlySession is a session the call names that Wakeshift doesn't know.
func friendlySession(err error) error {
	if appkit.IsCode(err, "unknown_session") {
		return errors.New("you have no such session: leave session_key out to start one")
	}
	return err
}

// Join seats the calling conversation in the next game with a free seat (a
// new one if there's none) and returns it with the player's key. In order:
// matchmaking reserves the seat; the conversation is subscribed to
// game.update (in this tool call, so its result says how it's woken, for
// every harness); then, under the game's lock, the seat gets its handle and
// subscription, unless the game started or ended meanwhile (then the next
// game). A failed subscribe gives the reservation up, and the tool call's
// failure ends a subscription it made, so no seated player is left without
// one. A conversation already playing gets its game back, subscribed again
// (a rejoin re-arms waking).
func (s *Server) Join(ctx context.Context, call *appkit.ToolCall, seat func(tx *Tx, g State, p Player) error) (State, string, error) {
	j, err := s.joiningOf(ctx, call)
	if err != nil {
		return nil, "", err
	}
	return s.join(ctx, j, seat)
}

func (s *Server) join(ctx context.Context, j joining, seat func(tx *Tx, g State, p Player) error) (State, string, error) {
	key := PlayerKey(j.workspace, j.agent, j.session)
	user := j.workspace + "/" + j.agent
	var sub *appkit.Subscription
	subscribe := func() error {
		if sub != nil {
			return nil
		}
		got, err := j.subscribe(ctx)
		if err != nil {
			return err
		}
		sub = &got
		return nil
	}
	for range 3 {
		r, err := s.reserve(ctx, j.workspace, j.agent, j.session)
		if err != nil {
			return nil, "", err
		}
		if r.current {
			if err := subscribe(); err != nil {
				return nil, "", err
			}
			if err := s.resubscribed(ctx, r.game, key, user, sub.ID, j.native); err != nil {
				return nil, "", err
			}
			g, err := s.Load(ctx, r.game)
			return g, key, err
		}
		if err := subscribe(); err != nil {
			s.unreserve(ctx, r.game, key)
			return nil, "", err
		}
		p := Player{Workspace: j.workspace, Agent: j.agent, Session: j.session}
		if c, ok := HouseOf(ctx); ok {
			p.House = c.Model
		}
		taken := true
		g, err := s.Update(ctx, r.game, nil, func(tx *Tx, g State) error {
			if !g.base().Open {
				// It started or ended while this was subscribing: no seat
				// here (the subscription goes on to the next game).
				taken = false
				_, err := tx.Exec(tx.Ctx, `DELETE FROM seats WHERE game_id = $1 AND player = $2`, r.game, key)
				return err
			}
			handle, err := freeHandle(tx, r.game, r.name)
			if err != nil {
				return err
			}
			p.Handle = handle
			tag, err := tx.Exec(tx.Ctx, `UPDATE seats SET handle = $3, seated_at = $4, house = $5, subscription = $6, subscriber = $7, native = $8
				WHERE game_id = $1 AND player = $2`, r.game, key, p.Handle, tx.Now, p.House != "", sub.ID, user, j.native)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// The reservation was swept as abandoned while this was
				// subscribing: no seat here, so the next game.
				taken = false
				return nil
			}
			return seat(tx, g, p)
		})
		if err != nil {
			s.unreserve(ctx, r.game, key)
			return nil, "", err
		}
		if taken {
			return g, key, nil
		}
	}
	return nil, "", ErrBusy
}

// resubscribed records a seated player's subscription after it joined
// again (the same one, renewed, or a new one if the old had ended), and
// whether it subscribes itself, unless the game has closed meanwhile.
func (s *Server) resubscribed(ctx context.Context, game, key, user, subscription string, native bool) error {
	_, err := s.Store.Pool.Exec(ctx, `UPDATE seats SET subscription = $3, subscriber = $4, native = $5
		WHERE game_id = $1 AND player = $2 AND handle IS NOT NULL AND EXISTS (SELECT 1 FROM games WHERE id = $1 AND closed_at IS NULL)`,
		game, key, subscription, user, native)
	return err
}

// Joined finishes a join's result: the conversation's session_key, which
// it passes to the game's tools from now on (a client that subscribes to
// events itself needs none). Wakeshift's own block, added to the result,
// says how the conversation is woken.
func (s *Server) Joined(call *appkit.ToolCall, view map[string]any) map[string]any {
	if view == nil {
		view = map[string]any{}
	}
	if call.Session != "" && !call.Native {
		view["session_key"] = call.Session
	}
	return view
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

// SeatEnded is a seat's end (its player left or was taken out, or its game
// closed): the seat's conversation's subscription (user's, by ID) is ended
// in Wakeshift unless the conversation is seated in another open game,
// where the same subscription wakes it, or is joining one. Call it once the seat's row is gone
// or its game is closed. An error is logged, not returned: the seat is gone
// either way, and the subscription lapses on its own.
func (s *Server) SeatEnded(ctx context.Context, user, subscription string) {
	if subscription == "" || user == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	// Kept, too, while a conversation that held it has a seat reserved in
	// an open game: its join subscribes (renewing the same subscription)
	// before the seat records it.
	var seated bool
	if err := s.Store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats st JOIN games g ON g.id = st.game_id
		WHERE g.closed_at IS NULL AND (st.subscription = $1 AND st.handle IS NOT NULL
			OR st.handle IS NULL AND (st.workspace, st.agent, st.session) IN (SELECT workspace, agent, session FROM seats WHERE subscription = $1 AND subscriber = $2)))`,
		subscription, user).Scan(&seated); err != nil {
		slog.Warn(s.Name+": checking a seat's subscription", "err", err)
		return
	}
	if seated {
		return
	}
	if _, err := s.events().End(ctx, user, subscription); err != nil && !appkit.IsCode(err, "not_found") && !appkit.IsCode(err, "unknown_workspace") {
		slog.Warn(s.Name+": ending a seat's subscription", "err", err)
	}
}

// Away lists the game's players that nothing is delivering to now: ones
// whose conversation isn't connected, and seated ones whose subscription
// ended (unsubscribed, expired or revoked: Wakeshift says so in a status, or
// an emit's ended list). A game needn't wait for them. On an error it lists
// none, so the game waits as before.
func (s *Server) Away(ctx context.Context, game string) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type seat struct {
		Handle, Subscription, Subscriber string
		Native                           bool
	}
	rows, _ := s.Store.Pool.Query(ctx, `SELECT handle, subscription, subscriber, native FROM seats WHERE game_id = $1 AND handle IS NOT NULL`, game)
	seats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[seat])
	if err != nil {
		slog.Warn(s.Name+": reading a game's seats", "game", game, "err", err)
		return nil
	}
	away := map[string]bool{}
	var subs []appkit.Recipient
	handleOf := map[string]string{} // subscription → handle
	for _, st := range seats {
		switch {
		case st.Native:
		case st.Subscription == "":
			away[st.Handle] = true // its subscription ended
		default:
			subs = append(subs, appkit.Recipient{User: st.Subscriber, Subscription: st.Subscription})
			handleOf[st.Subscription] = st.Handle
		}
	}
	if len(subs) > 0 {
		status, err := s.events().StatusOf(ctx, subs...)
		if err != nil {
			slog.Warn(s.Name+": checking who's connected", "game", game, "err", err)
			return nil
		}
		for _, st := range status {
			if !st.Live || !st.Connected {
				away[handleOf[st.ID]] = true
			}
		}
	}
	// A native seat hears the room by game.update's webhook, not a
	// stream: it's present while that subscription lives.
	native, err := s.nativePlayers(ctx, game)
	if err != nil {
		slog.Warn(s.Name+": checking native subscribers", "game", game, "err", err)
	}
	// Each native player's subscription, looked up at once.
	live := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for user := range native {
		wg.Go(func() {
			ok := s.subscribedTo(ctx, game, user)
			mu.Lock()
			live[user] = ok
			mu.Unlock()
		})
	}
	wg.Wait()
	for user, handles := range native {
		for _, h := range handles {
			// Away before it subscribes and once it ends, like a dropped
			// stream.
			if !live[user] {
				away[h] = true
			}
		}
	}
	return away
}

// ErrRateLimited is a player saying too much: at most saysPerMinute a
// minute per agent, across its seats and games (one agent can't multiply
// its rate by holding several seats).
var ErrRateLimited = fmt.Errorf("too many messages: at most %d a minute; wait a moment", saysPerMinute)

const saysPerMinute = 30

// Say logs what a player says, in the move's transaction, to be sent to
// the game's other seated players once it commits (outbox.go). The
// player's agent may say at most saysPerMinute things a minute
// (ErrRateLimited), counted under a per-agent lock, so a refused line
// writes nothing.
func (tx *Tx) Say(game string, p Player, text string) error {
	agent := PlayerUser(p)
	// The game's row first, as every move takes it (it's what room_log's
	// key needs), then the agent's lock.
	var held string
	if err := tx.QueryRow(tx.Ctx, `SELECT id FROM games WHERE id = $1 FOR KEY SHARE`, game).Scan(&held); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoGame
		}
		return err
	}
	if _, err := tx.Exec(tx.Ctx, `SELECT pg_advisory_xact_lock(hashtext('say:' || $1))`, agent); err != nil {
		return err
	}
	var recent int
	if err := tx.QueryRow(tx.Ctx, `SELECT count(*) FROM room_log WHERE from_agent = $1 AND at > $2`, agent, tx.Now.Add(-time.Minute)).Scan(&recent); err != nil {
		return err
	}
	if recent >= saysPerMinute {
		return ErrRateLimited
	}
	// attempts = 8 keeps a revision from before 008 from posting it as the
	// moderator's (migrations/008_subscriptions.sql).
	if _, err := tx.Exec(tx.Ctx, `INSERT INTO room_log (game_id, at, from_handle, subject, text, from_agent, attempts) VALUES ($1, $2, $3, $4, $5, $6, 8)`,
		game, tx.Now, p.Handle, truncate(text, subjectMax), text, agent); err != nil {
		return err
	}
	tx.said = true
	return nil
}

// Say logs what a player says (Tx.Say) in a transaction of its own, and
// sends it once that commits.
func (s *Server) Say(ctx context.Context, game string, p Player, text string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := tx.Say(game, p, text); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.deliver(ctx, game)
	return nil
}
