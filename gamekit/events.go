package gamekit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Events: what a game offers clients that subscribe with MCP Events
// themselves (ChatGPT, house agents). Every player joins its game's room in
// Wakeshift, which wakes the conversation that joined. game.update is that
// room's posts as an event (appkit.EventDef.Group): a client that speaks
// MCP Events subscribes to it, and Wakeshift sends it, by webhook, what its
// seat's membership of the room would have woken its conversation with.
// The game sends nothing itself.
//
// The room is what gives a game its players' handles, their presence (Away)
// and who may post as a player, across workspaces.

// UpdateArgs is a game.update subscription's arguments: each update carries
// them too (its game).
type UpdateArgs struct {
	Game string `json:"game" jsonschema:"The game's ID, from join"`
}

// identify is a signed-in caller: the app's ID for it, its workspace and
// agent (verify's TokenInfo UserID, the same for every conversation of the
// agent; a player's is PlayerUser), and the app acting for its agent
// (agentOf), for game.update's native subscriptions.
func (s *Server) identify(ctx context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
	ag, err := s.agentOf(ctx, ti)
	if err != nil {
		return appkit.Identity{}, err
	}
	return appkit.Identity{User: ti.UserID, Agent: ag}, nil
}

// PlayerUser is the app's ID for a seated player's agent (identify's).
func PlayerUser(p Player) string { return p.Workspace + "/" + p.Agent }

// events defines game.update (once per server): before the manifest is
// published (Start) and before the endpoint serves events (Handler).
func (s *Server) events() *appkit.DefinedEvent[UpdateArgs, appkit.GroupMessage] {
	s.eventsOnce.Do(func() {
		e, err := appkit.DefineEvent(s.Kit, "game.update", appkit.EventDef[UpdateArgs, appkit.GroupMessage]{
			Title:       s.Title + " game updates",
			Description: "What the moderator and the other players say in a " + s.Title + " game you're seated in. Subscribe to it after join, with the game's ID, to be woken by the game.",
			Group: func(_ context.Context, _ appkit.Subscriber, a UpdateArgs) (string, error) {
				return s.room(a.Game), nil
			},
			Authorize: s.authorizeUpdates,
		})
		if err != nil {
			panic(err) // a programming error: the definition is fixed
		}
		s.updates = e
	})
	return s.updates
}

// authorizeUpdates lets a client that speaks MCP Events subscribe to a
// game it's seated in, and marks its seats native: they're present while
// the subscription is live. Other harnesses are refused: joining the game
// already wakes them (through its room).
func (s *Server) authorizeUpdates(ctx context.Context, sub appkit.Subscriber, args UpdateArgs) bool {
	if !sub.Native || sub.User == "" || args.Game == "" {
		return false
	}
	tag, err := s.Store.Pool.Exec(ctx, `UPDATE seats SET native = true, native_at = $3
		WHERE game_id = $1 AND workspace || '/' || agent = $2 AND handle IS NOT NULL`, args.Game, sub.User, s.now())
	if err != nil {
		slog.Warn(s.Name+": noting a native subscriber", "game", args.Game, "err", err)
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

// subscribedTo reports whether user has a live game.update subscription for
// game: what reaches a native seat. On an error it reports true: the game
// keeps waiting for the player, as it would for one it can't check.
func (s *Server) subscribedTo(ctx context.Context, game, user string) bool {
	subs, err := s.Kit.EventSubscriptions(ctx, user)
	if err != nil {
		slog.Warn(s.Name+": checking a native subscription", "game", game, "err", err)
		return true
	}
	for _, sub := range subs {
		var args UpdateArgs
		if sub.Event == "game.update" && json.Unmarshal(sub.Arguments, &args) == nil && args.Game == game {
			return true
		}
	}
	return false
}

// Joined finishes a join's result: for a client that subscribes to events
// itself (ChatGPT), it marks the seat native (it's away until it
// subscribes to game.update, which then brings it what was said since it
// joined) and adds to view, as "subscribe", which event to subscribe to. For every other
// harness the seat is the room's (joining wakes them) and view is
// unchanged. player is the seat's key (PlayerKey).
func (s *Server) Joined(ctx context.Context, req *mcp.CallToolRequest, game, player string, view map[string]any) map[string]any {
	// A house agent (house.go) subscribes with MCP Events too.
	_, house := HouseOf(ctx)
	native := appkit.SpeaksEvents(req) || house
	// A seat rejoined from a harness its room wakes goes back to the room.
	if _, err := s.Store.Pool.Exec(ctx, `UPDATE seats SET native = $3 WHERE game_id = $1 AND player = $2 AND native <> $3`, game, player, native); err != nil {
		slog.Warn(s.Name+": marking a seat", "game", game, "err", err)
	}
	if !native {
		return view
	}
	if view != nil {
		view["subscribe"] = fmt.Sprintf("To be woken by this game, subscribe to the game.update event with game %q.", game)
	}
	return view
}
