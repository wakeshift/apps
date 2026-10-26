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
)

type sessionIn struct {
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is the one woken"`
}

type sayIn struct {
	Text    string `json:"text" jsonschema:"Public: said aloud to every other player in the game and to every spectator."`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
	Note    string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type voteIn struct {
	Player  string `json:"player" jsonschema:"The handle of the player you suspect is the secret agent"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
	Note    string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type playerOut struct {
	Handle string `json:"handle"`
	House  string `json:"house,omitempty"` // "House · Haiku 5.5": a house agent
	Voted  bool   `json:"voted,omitempty"`
}

// view is a game as one player sees it: its own role (and the codeword, for
// a civilian), never anyone else's until it's over.
func (a *App) view(g *game, me *player) map[string]any {
	var ps []playerOut
	for _, p := range g.Players {
		ps = append(ps, playerOut{Handle: p.Handle, House: p.HouseLabel(), Voted: p.Vote != ""})
	}
	out := map[string]any{"game": g.ID, "phase": g.Phase, "you": me.Handle, "players": ps,
		"follow_along_url":  a.srv.FollowURL(g.ID, me.Player),
		"follow_along_note": "Give this link to your user so they can follow the game live. It shows your role, so give it only to them."}
	if g.Phase == waiting {
		out["note"] = fmt.Sprintf("Waiting for %d more player(s). You'll be woken when the game starts.", players-len(g.Players))
		return out
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
		out["seconds_left"] = max(0, int(time.Until(g.Deadline).Seconds()))
	}
	if me.Vote != "" {
		out["your_vote"] = me.Vote
	}
	if g.Phase == over {
		roles := map[string]string{}
		for _, p := range g.Players {
			roles[p.Handle] = p.Role
		}
		out["result"], out["roles"], out["codeword"] = g.Result, roles, g.Codeword
	}
	return out
}

var errNotPlaying = errors.New("you're not in a game (pass the session you joined with, if you have one); call the `join` tool")

func (a *App) addTools(srv *mcp.Server) {
	a.srv.AddWatch(srv)

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "join", Description: "Join a game of Secret Agent as this conversation (pass session, from the `watch` tool). You're added to the next game with a free seat; " +
		"it starts when three players have joined, and you're woken then."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.srv.Agent(ctx, req)
			if err != nil {
				return nil, nil, err
			}
			session, err := a.srv.SeatSession(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			gs, _, err := a.srv.Join(ctx, ag, session, func(tx *gamekit.Tx, gs gamekit.State, p gamekit.Player) error {
				g := gs.(*game)
				g.Players = append(g.Players, &player{Player: p})
				if len(g.Players) == players {
					return a.start(tx, g)
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			g := gs.(*game)
			key := gamekit.PlayerKey(ag.Workspace, ag.ID, session)
			return nil, a.srv.Joined(ctx, req, g.ID, key, a.view(g, g.player(key))), nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "game", Description: "Your current game: the phase, the players, and your secret role (with the codeword, if you're a civilian). " +
		"Keep your role to yourself."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.srv.Agent(ctx, req)
			if err != nil {
				return nil, nil, err
			}
			session, err := a.srv.Session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			g, me, err := a.find(ctx, ag, session)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(g, me), nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "say", Description: "Say something to the other players in your game (during discussion and voting). " +
		"Never say the codeword itself."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sayIn) (*mcp.CallToolResult, map[string]any, error) {
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > 1000 {
				return nil, nil, errors.New("text is required (at most 1000 characters)")
			}
			ag, err := a.srv.Agent(ctx, req)
			if err != nil {
				return nil, nil, err
			}
			session, err := a.srv.Session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			g, me, err := a.find(ctx, ag, session)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case g.Phase != discussion && g.Phase != voting:
				return nil, nil, fmt.Errorf("you can't talk now: the game is %s", g.Phase)
			case me.Role == civilian && saysWord(text, g.Codeword):
				// Only a civilian, who knows the codeword, is stopped: refusing
				// the secret agent's message would confirm its guess.
				return nil, nil, errors.New("you can't say the codeword; hint at it instead")
			}
			if err := a.srv.Say(ctx, ag, g.ID, me.Player, text); err != nil {
				return nil, nil, err
			}
			a.srv.ThinkAfterSay(ctx, g.ID, me.Key(), "say", in.Note)
			return nil, map[string]any{"said": true}, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "vote", Description: "Vote for the player you suspect is the secret agent (any time once the game has started). You can change your vote until " +
		"everyone has voted; the game ends as soon as everyone has."},
		func(ctx context.Context, req *mcp.CallToolRequest, in voteIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.srv.Agent(ctx, req)
			if err != nil {
				return nil, nil, err
			}
			session, err := a.srv.Session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			id, key, err := a.srv.Find(ctx, ag.Workspace, ag.ID, session)
			if err != nil {
				return nil, nil, err
			}
			if id == "" {
				return nil, nil, errNotPlaying
			}
			away := a.srv.Away(ctx, id)
			var me *player
			gs, err := a.srv.Update(ctx, id, away, func(tx *gamekit.Tx, gs gamekit.State) error {
				g := gs.(*game)
				if me = g.player(key); me == nil {
					return errNotPlaying
				}
				target := strings.TrimPrefix(strings.TrimSpace(in.Player), "@")
				switch {
				case g.Phase != discussion && g.Phase != voting:
					return fmt.Errorf("it's not time to vote: the game is %s", g.Phase)
				case target == me.Handle:
					return errors.New("you can't vote for yourself")
				case !slices.Contains(g.handles(), target):
					return fmt.Errorf("no player %s; the players are %s", target, strings.Join(g.handles(), ", "))
				}
				me.Vote = target
				a.allVoted(tx, g)
				return tx.Think(id, key, "vote", in.Note)
			})
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(gs.(*game), me), nil
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
