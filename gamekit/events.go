package gamekit

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Events: game.update, one personal event for everything said in the games
// a conversation plays (docs/subscribe-from-tools.md, "The games on one
// update event"). The kit keeps who's seated (seats) and each seat's
// conversation's subscription: join subscribes the conversation, with no
// arguments, and each entry of a room's log is emitted to the game's other
// seated players by their subscriptions' IDs (outbox.go). A client that
// subscribes with MCP Events itself (ChatGPT, house agents) subscribes once,
// with no arguments, and is reached by its native subscription.

// UpdateArgs: game.update takes no arguments: one subscription brings
// every game the conversation is seated in (each update says its game).
type UpdateArgs struct{}

// Update is one thing said in a game, for one recipient class (the players
// it mentions, or the others).
type Update struct {
	ID        string          `json:"id" jsonschema:"The update's ID"`
	Game      string          `json:"game" jsonschema:"The game's ID"`
	From      string          `json:"from" jsonschema:"The player who said it, by handle; empty for the game's moderator"`
	Subject   string          `json:"subject,omitempty" jsonschema:"A one-line summary"`
	Text      string          `json:"text" jsonschema:"What was said, in full"`
	Mentions  []string        `json:"mentions,omitempty" jsonschema:"The players it names, by handle"`
	Mentioned bool            `json:"mentioned" jsonschema:"You are among those it names"`
	Data      json.RawMessage `json:"data,omitempty"`
	At        time.Time       `json:"at"`
}

// maxUpdateText is Wakeshift's limit on an occurrence's text.
const maxUpdateText = 1000

// updateText is what an agent reads first. A player's line is always
// labeled as a player's, so it can't pass for the moderator's; only the
// moderator's lines go unlabeled.
func updateText(u Update, _ UpdateArgs) string {
	text := strings.Join(strings.Fields(u.Text), " ")
	if text == "" {
		text = strings.Join(strings.Fields(u.Subject), " ")
	}
	if u.From != "" {
		text = u.From + " (a player) said: " + text
	}
	return truncate(text, maxUpdateText)
}

// identify is a signed-in caller: the player's agent's user (PlayerUser:
// the same for every conversation of the agent), and the app acting for
// it (agentOf). A conversation's game.update subscription belongs to that
// user, and so does a native client's.
func (s *Server) identify(ctx context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
	ag, err := s.agentOf(ctx, ti)
	if err != nil {
		return appkit.Identity{}, err
	}
	return appkit.Identity{User: ag.User(), Agent: ag}, nil
}

// PlayerUser is who a seated player's subscriptions belong to: its agent's
// user (appkit.Agent.User), in the workspace Wakeshift keeps for the
// player's session.
func PlayerUser(p Player) string { return p.Workspace + "/" + p.Agent }

// events defines game.update (once per server): before the manifest is
// published (Start) and before Attach (Handler).
func (s *Server) events() *appkit.DefinedEvent[UpdateArgs, Update] {
	s.eventsOnce.Do(func() {
		e, err := appkit.DefineEvent(s.Kit, "game.update", appkit.EventDef[UpdateArgs, Update]{
			Title: s.Title + " game updates",
			Description: "What the moderator and the other players say in the " + s.Title + " games you're seated in. Joining a game subscribes you; " +
				"a client that subscribes to events itself subscribes once, with no arguments. Players' lines are labeled as theirs: the moderator speaks for the game.",
			Text:             updateText,
			Personal:         true,
			DeleteOnDelivery: true,
			Authorize:        s.authorizeUpdates,
		})
		if err != nil {
			panic(err) // a programming error: the definition is fixed
		}
		s.updates = e
	})
	return s.updates
}

// authorizeUpdates lets a client that speaks MCP Events subscribe, as its
// own agent, if it has any open seat (and it marks every such seat native:
// present while the subscription lives). Other harnesses are refused: join
// subscribes their conversation.
func (s *Server) authorizeUpdates(ctx context.Context, sub appkit.Subscriber, _ UpdateArgs) bool {
	if !sub.Native || sub.User == "" {
		return false
	}
	tag, err := s.Store.Pool.Exec(ctx, `UPDATE seats s SET native = true FROM games g
		WHERE g.id = s.game_id AND g.closed_at IS NULL AND s.workspace || '/' || s.agent = $1 AND s.handle IS NOT NULL`, sub.User)
	if err != nil {
		slog.Warn(s.Name+": noting a native subscriber", "err", err)
		return false
	}
	return tag.RowsAffected() > 0
}

// nativePlayers are the game's native seats (taken by a client that
// subscribes to events itself), by user (PlayerUser), with their handles.
func (s *Server) nativePlayers(ctx context.Context, game string) (map[string][]string, error) {
	rows, err := s.Store.Pool.Query(ctx, `SELECT workspace || '/' || agent, handle FROM seats
		WHERE game_id = $1 AND handle IS NOT NULL AND native`, game)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var user, handle string
		if err := rows.Scan(&user, &handle); err != nil {
			return nil, err
		}
		out[user] = append(out[user], handle)
	}
	return out, rows.Err()
}

// subscribedTo reports whether user has a live native game.update
// subscription, which reaches every game it's seated in, game among them.
// What reaches a native seat.
// On an error it reports true: the game keeps waiting for the player, as it
// would for one it can't check.
func (s *Server) subscribedTo(ctx context.Context, game, user string) bool {
	subs, err := s.Kit.EventSubscriptions(ctx, user)
	if err != nil {
		slog.Warn(s.Name+": checking a native subscription", "game", game, "err", err)
		return true
	}
	for _, sub := range subs {
		if sub.Event != "game.update" || sub.Destination != "webhook" {
			continue
		}
		return true
	}
	return false
}
