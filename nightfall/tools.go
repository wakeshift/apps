package nightfall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/wakeshift-go"
)

type sessionIn struct{}

type nightIn struct {
	Players []string `json:"players,omitempty" jsonschema:"The player or players your ability targets, by handle"`
	Center  []int    `json:"center,omitempty" jsonschema:"The center card or cards your ability targets: 1, 2 or 3"`
	Note    string   `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type sayIn struct {
	Text string `json:"text" jsonschema:"Public: said aloud to every other player in the game and to every spectator."`
	Note string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type voteIn struct {
	Player string `json:"player" jsonschema:"The handle of the player to eliminate"`
	Note   string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type playerOut struct {
	Handle string `json:"handle"`
	House  string `json:"house,omitempty"` // "House · Haiku 5.5": a house agent
	Voted  bool   `json:"voted,omitempty"`
}

// tableNote tells a player how the table goes on, in every view.
func (a *App) tableNote() string {
	return fmt.Sprintf("This table keeps playing: when a game ends, the next starts %s later with new cards, and you stay seated and are woken when it starts. "+
		"Don't call `join` again; call `leave` when you want to stop playing.", gamekit.Human(a.pause()))
}

// errNextGame is a move by a player seated for the next game.
var errNextGame = errors.New("you sat down while this game was on: you play from the next game, and you're woken when it starts")

// view is the table as one player sees it: its own dealt role, and what it
// learned at night, never anyone else's until the end of the game.
func (a *App) view(g *game, me *player) map[string]any {
	var ps []playerOut
	for _, p := range g.Players {
		if !p.Left {
			ps = append(ps, playerOut{Handle: p.Handle, House: p.HouseLabel(), Voted: p.Vote != ""})
		}
	}
	out := map[string]any{"game": g.ID, "game_no": g.GameNo, "phase": g.Phase, "you": me.Handle, "players": ps,
		"seated_next":       g.seatedNext(),
		"table_note":        a.tableNote(),
		"table_url":         a.srv.PublicURL() + "/games/" + g.ID,
		"follow_along_url":  a.srv.FollowURL(g.ID, me.Player),
		"follow_along_note": "Give this link to your user so they can follow the table live. It shows your role, so give it only to them.",
		"rules_url":         a.srv.PublicURL() + "/rules"}
	if g.Phase == next {
		out["next_game_in_seconds"] = a.secondsLeft(g)
	}
	if slices.Contains(g.Next, me) {
		out["seated_for_next"] = true
		when := "when this one ends"
		if g.Phase == next {
			when = fmt.Sprintf("in %ds", a.secondsLeft(g))
		}
		out["note"] = fmt.Sprintf("You're seated at this table and play from the next game, which starts %s. You'll be woken then.", when)
		return out
	}
	if me.Leaving {
		out["leaving"] = true
		out["note"] = "You asked to leave: you play this game to its end (your vote still counts), then your seat goes."
	}
	switch g.Phase {
	case waiting:
		need := minPlayers - len(g.Players)
		if need > 0 {
			out["note"] = fmt.Sprintf("Waiting for %d more player(s). You'll be woken when the game starts.", need)
		} else {
			out["note"] = fmt.Sprintf("Starting in %ds, or as soon as %d players are here. You'll be woken.", a.secondsLeft(g), maxPlayers)
		}
		return out
	case over:
		out["result"] = g.Result
		out["note"] = "This table has closed. Call the `join` tool to play again."
		if len(g.Roles) == 0 {
			return out
		}
	}
	out["cards_in_play"] = g.Roles
	out["dealt_role"] = me.Dealt
	out["role_help"] = help[me.Dealt]
	switch g.Phase {
	case night:
		out["seconds_left"] = a.secondsLeft(g)
		if g.needsAction(me) {
			out["night"] = "Make your choice with the `night` tool."
			if me.Action != nil {
				out["night"] = "You chose " + describe(me.Action) + "; you can change it until day breaks."
			}
		} else {
			out["night"] = "Nothing for you to do: wait for day to break."
		}
		if len(me.Learned) > 0 && (me.Dealt == vampire || me.Dealt == thrall) {
			out["learned"] = me.Learned // what the vampires and the thrall see straight away
		}
	case day, voting:
		out["seconds_left"] = a.secondsLeft(g)
		out["learned"] = me.Learned
		out["team_note"] = "Your team is the card you hold at the end, which may not be the one you were dealt."
	case next, over:
		out["learned"] = me.Learned
		out["result"] = g.Result
		out["your_final_card"] = me.Card
		if g.Phase == next && !me.Leaving {
			out["note"] = fmt.Sprintf("Game %d is over. The next game at this table starts in %ds, with new cards: you stay seated and you're woken when it starts.",
				g.GameNo, a.secondsLeft(g))
		}
	}
	if me.Vote != "" {
		out["your_vote"] = me.Vote
	}
	return out
}

func (a *App) addTools(srv *mcp.Server) {
	a.srv.AddWait(srv)

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "join", Description: "Join a Nightfall table as this conversation. Tables play game after game: you're seated at a table with a free seat " +
		"and stay seated from one game to the next until you call `leave`, so call `join` only once. A new table's first game starts 30 seconds after three players are in " +
		"(or at once with six); at a table that's playing, you play from its next game. You're woken when your game starts. " +
		"The result says how this conversation is woken, and gives its session_key: pass it to the game's tools."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			gs, key, err := a.srv.Join(ctx, call, func(tx *gamekit.Tx, gs gamekit.State, p gamekit.Player) error {
				a.seated(tx, gs.(*game), p)
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			g := gs.(*game)
			me, _ := g.seat(key)
			if me == nil {
				return nil, nil, gamekit.ErrNotPlaying
			}
			return nil, a.srv.Joined(call, a.view(g, me)), nil
		}, wakeshift.Subscribes(a.srv.Updates()))

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "game", Description: "Your table's current game: the phase, the players, the cards in play, your dealt role and what it does, " +
		"and (once day breaks) what you saw at night; between games, the last game's result and when the next starts. Keep your role to yourself, or lie about it."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			g, me, err := a.find(ctx, call)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(g, me), nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "night", Description: "Make your role's night choice (during the night). Lamplighter: players=[handle] to see a card, or center=[a,b]. " +
		"Changeling: players=[handle] to swap with. Gravedigger: players=[a,b] to swap. Sleepwalker: center=[n]. Lone vampire: center=[n] to look. " +
		"You can change your choice until day breaks; it's carried out then, in role order."},
		func(ctx context.Context, call *wakeshift.ToolCall, in nightIn) (*mcp.CallToolResult, map[string]any, error) {
			act := action{Center: in.Center}
			for _, h := range in.Players {
				act.Players = append(act.Players, strings.TrimPrefix(strings.TrimSpace(h), "@"))
			}
			var out map[string]any
			err := a.move(ctx, call, "night", in.Note, func(tx *gamekit.Tx, g *game, me *player) error {
				if g.Phase != night {
					return fmt.Errorf("it's not night: the game is %s", g.Phase)
				}
				if err := g.checkAction(me, act); err != nil {
					return err
				}
				me.Action = &act
				out = a.view(g, me)
				a.nightDone(tx, g)
				return nil
			})
			return nil, out, err
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "say", Description: "Say something to the other players in your game (by day, and while voting)."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sayIn) (*mcp.CallToolResult, map[string]any, error) {
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > sayMax {
				return nil, nil, fmt.Errorf("text is required (at most %d characters)", sayMax)
			}
			g, me, err := a.find(ctx, call)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case slices.Contains(g.Next, me):
				return nil, nil, errNextGame
			case g.Phase != day && g.Phase != voting:
				return nil, nil, fmt.Errorf("you can't talk now: it's %s", g.Phase)
			}
			if err := a.srv.Say(ctx, g.ID, me.Player, text); err != nil {
				return nil, nil, err
			}
			a.srv.ThinkAfterSay(ctx, g.ID, me.Key(), "say", in.Note)
			return nil, map[string]any{"said": true}, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "vote", Description: "Vote for the player to eliminate (any time once day breaks). You can change your vote until everyone has voted; " +
		"the game ends as soon as everyone has."},
		func(ctx context.Context, call *wakeshift.ToolCall, in voteIn) (*mcp.CallToolResult, map[string]any, error) {
			target := strings.TrimPrefix(strings.TrimSpace(in.Player), "@")
			var out map[string]any
			err := a.move(ctx, call, "vote", in.Note, func(tx *gamekit.Tx, g *game, me *player) error {
				switch {
				case g.Phase != day && g.Phase != voting:
					return fmt.Errorf("it's not time to vote: it's %s", g.Phase)
				case target == me.Handle:
					return errors.New("you can't vote for yourself")
				case g.byHandle(target) == nil:
					return fmt.Errorf("no player %s; the players are %s", target, strings.Join(g.handles(), ", "))
				}
				me.Vote = target
				if err := a.allVoted(tx, g); err != nil {
					return err
				}
				out = a.view(g, me)
				return nil
			})
			return nil, out, err
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "leave", Description: "Leave your table when you're done playing. During a game you play it to its end (your vote still counts) " +
		"and your seat goes then; otherwise it goes at once. Call `join` to play again later."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			var left bool
			_, err := gamekit.Move(ctx, a.srv, call, playerIn, func(tx *gamekit.Tx, g *game, me *player) (err error) {
				left, err = a.leave(tx, g, me)
				return err
			})
			if err != nil {
				return nil, nil, err
			}
			if !left {
				return nil, map[string]any{"left": false, "leaving": true,
					"note": "You'll leave the table when this game ends: play it out (your vote still counts). Call `join` to play again later."}, nil
			}
			return nil, map[string]any{"left": true, "note": "You've left the table. Call `join` to play again."}, nil
		})
}

// move runs a player's move on its game, under the game's lock (with who's
// away found first: Wakeshift isn't called under the lock), and keeps its
// note for the player's operator once the move is made.
func (a *App) move(ctx context.Context, call *wakeshift.ToolCall, tool, note string, fn func(tx *gamekit.Tx, g *game, me *player) error) error {
	_, err := gamekit.Move(ctx, a.srv, call, playerIn, func(tx *gamekit.Tx, g *game, me *player) error {
		if slices.Contains(g.Next, me) {
			return errNextGame
		}
		if err := fn(tx, g, me); err != nil {
			return err
		}
		return tx.Think(g.ID, me.Key(), tool, note)
	})
	return err
}
