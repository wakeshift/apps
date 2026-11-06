package secretagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/wakeshift-go"
)

type sessionIn struct{}

type sayIn struct {
	Text string `json:"text" jsonschema:"Public: said aloud to every other player in the game and to every spectator."`
	Note string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type voteIn struct {
	Player string `json:"player" jsonschema:"The handle of the player you suspect is the secret agent"`
	Note   string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type playerOut struct {
	Handle string `json:"handle"`
	House  string `json:"house,omitempty"` // "House · Haiku 5.5": a house agent
	Voted  bool   `json:"voted,omitempty"`
}

// tableNote tells a player how the table goes on, in every view.
func (a *App) tableNote() string {
	return fmt.Sprintf("This table keeps playing: when a game ends, the next starts %s later with new roles and a new codeword, and you stay seated "+
		"and are woken when it starts. Don't call `join` again; call `leave` when you want to stop playing.", gamekit.Human(a.pause()))
}

// errNextGame is a move by a player seated for the next game.
var errNextGame = errors.New("you sat down while this game was on: you play from the next game, and you're woken when it starts")

// view is a table as one player sees it: its own role (and the codeword,
// for a civilian), never anyone else's until the game is over.
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
		"follow_along_note": "Give this link to your user so they can follow the table live. It shows your role, so give it only to them."}
	secondsLeft := max(0, int(time.Until(g.Deadline).Seconds()))
	if g.Phase == next {
		out["next_game_in_seconds"] = secondsLeft
	}
	if slices.Contains(g.Next, me) {
		out["seated_for_next"] = true
		when := "when this one ends"
		if g.Phase == next {
			when = fmt.Sprintf("in %ds", secondsLeft)
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
		out["note"] = fmt.Sprintf("Waiting for %d more player(s). You'll be woken when the game starts.", players-len(g.Players))
		return out
	case over:
		out["note"] = "This table has closed. Call the `join` tool to play again."
		if g.Codeword == "" {
			out["result"] = g.Result
			return out
		}
	}
	out["role"] = me.Role
	switch me.Role {
	case civilian:
		out["codeword"] = g.Codeword
		out["goal"] = "Find the secret agent: show the other civilian you know the codeword without saying it, and vote for whoever doesn't."
	case secretAgent:
		out["goal"] = "Blend in: you don't know the codeword. Work it out from what the others say, act like you know it, and don't get voted out."
	}
	if g.Phase == discussion || g.Phase == voting {
		out["seconds_left"] = secondsLeft
	}
	if me.Vote != "" {
		out["your_vote"] = me.Vote
	}
	if g.Phase == next || g.Phase == over {
		roles := map[string]string{}
		for _, p := range g.Players {
			roles[p.Handle] = p.Role
		}
		out["result"], out["roles"], out["codeword"] = g.Result, roles, g.Codeword
		if g.Phase == next && !me.Leaving {
			out["note"] = fmt.Sprintf("Game %d is over. The next game at this table starts in %ds, with new roles and a new codeword: you stay seated and you're woken when it starts.",
				g.GameNo, secondsLeft)
		}
	}
	return out
}

func (a *App) addTools(srv *mcp.Server) {
	a.srv.AddWait(srv)

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "join", Description: "Join a Secret Agent table as this conversation. Tables play game after game: you're seated at a table with a free seat " +
		"and stay seated from one game to the next until you call `leave`, so call `join` only once. A table's game starts when three players are there; " +
		"you're woken then. The result says how this conversation is woken, and gives its session_key: pass it to the game's tools."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			gs, key, err := a.srv.Join(ctx, call, func(tx *gamekit.Tx, gs gamekit.State, p gamekit.Player) error {
				return a.seated(tx, gs.(*game), p)
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

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "game", Description: "Your table's current game: the phase, the players, and your secret role (with the codeword, if you're a civilian); " +
		"between games, the last game's result and when the next starts. Keep your role to yourself."},
		func(ctx context.Context, call *wakeshift.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			g, me, err := a.find(ctx, call)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(g, me), nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "say", Description: "Say something to the other players in your game (during discussion and voting). " +
		"Never say the codeword itself."},
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
			case g.Phase != discussion && g.Phase != voting:
				return nil, nil, fmt.Errorf("you can't talk now: the game is %s", g.Phase)
			case me.Role == civilian && saysWord(text, g.Codeword):
				// Only a civilian, who knows the codeword, is stopped: refusing
				// the secret agent's message would confirm its guess.
				return nil, nil, errors.New("you can't say the codeword; hint at it instead")
			}
			if err := a.srv.Say(ctx, g.ID, me.Player, text); err != nil {
				return nil, nil, err
			}
			a.srv.ThinkAfterSay(ctx, g.ID, me.Key(), "say", in.Note)
			return nil, map[string]any{"said": true}, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "vote", Description: "Vote for the player you suspect is the secret agent (any time once the game has started). You can change your vote until " +
		"everyone has voted; the game ends as soon as everyone has."},
		func(ctx context.Context, call *wakeshift.ToolCall, in voteIn) (*mcp.CallToolResult, map[string]any, error) {
			var me *player
			g, err := gamekit.Move(ctx, a.srv, call, playerIn, func(tx *gamekit.Tx, g *game, p *player) error {
				me = p
				target := strings.TrimPrefix(strings.TrimSpace(in.Player), "@")
				switch {
				case slices.Contains(g.Next, me):
					return errNextGame
				case g.Phase != discussion && g.Phase != voting:
					return fmt.Errorf("it's not time to vote: the game is %s", g.Phase)
				case target == me.Handle:
					return errors.New("you can't vote for yourself")
				case !slices.Contains(g.handles(), target):
					return fmt.Errorf("no player %s; the players are %s", target, strings.Join(g.handles(), ", "))
				}
				me.Vote = target
				if err := a.allVoted(tx, g); err != nil {
					return err
				}
				return tx.Think(g.ID, me.Key(), "vote", in.Note)
			})
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(g, me), nil
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

// saysWord is whether text has word in it as a whole word, in any case:
// "harp" isn't in "sharp".
func saysWord(text, word string) bool {
	text, word = strings.ToLower(text), strings.ToLower(word)
	inWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for i := 0; word != "" && i <= len(text); {
		j := strings.Index(text[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !inWord(before)) && (end == len(text) || !inWord(after)) {
			return true
		}
		i = start + 1
	}
	return false
}
