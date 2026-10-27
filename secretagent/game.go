package secretagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

const (
	players = 3

	civilian    = "civilian"
	secretAgent = "secret agent"

	waiting    = "waiting for players"
	discussion = "discussion"
	voting     = "voting"
	over       = "over"
)

// player is a seat in a game, and what only the app knows about it.
type player struct {
	gamekit.Player
	Role string `json:"role,omitempty"`
	Vote string `json:"vote,omitempty"` // the handle voted for
}

// game is one game of Secret Agent: the kit's part (its ID, phase and
// deadline), and the state only the app knows (roles, the codeword,
// votes). It's kept as JSON in the database.
type game struct {
	gamekit.Game
	Players  []*player `json:"players"`
	Codeword string    `json:"codeword,omitempty"`
	Result   string    `json:"result,omitempty"`
	// Started is when the discussion opened: archived moves are timed from it.
	Started time.Time `json:"started,omitzero"`
}

func (g *game) handles() []string {
	var out []string
	for _, p := range g.Players {
		out = append(out, p.Handle)
	}
	return out
}

func (g *game) player(key string) *player {
	for _, p := range g.Players {
		if p.Key() == key {
			return p
		}
	}
	return nil
}

// recentWords is how many of each player's last codewords a game avoids.
const recentWords = 50

// pickCodeword picks a word none of the players has had in its last
// recentWords games, and adds it to what they've had (recent, by player
// key).
func pickCodeword(recent map[string][]string, ps []*player) string {
	var fresh []string
	for _, w := range codewords {
		used := false
		for _, p := range ps {
			used = used || slices.Contains(recent[p.Key()], w)
		}
		if !used {
			fresh = append(fresh, w)
		}
	}
	if len(fresh) == 0 {
		fresh = codewords
	}
	w := fresh[gamekit.RandInt(len(fresh))]
	for _, p := range ps {
		r := append(recent[p.Key()], w)
		recent[p.Key()] = r[max(0, len(r)-recentWords):]
	}
	return w
}

// step is the game's deadline passing.
func (a *App) step(tx *gamekit.Tx, g *game) error {
	switch g.Phase {
	case waiting:
		g.Phase, g.Result = over, "Nobody else joined."
		g.Announce("Game closed", fmt.Sprintf("Nobody else joined in %s, so this game is closed. Call the `join` tool to try again.", gamekit.Human(a.wait())), g.handles())
		g.End(time.Minute)
	case discussion:
		return a.openVoting(tx, g)
	case voting:
		return a.finish(tx, g)
	}
	return nil
}

// start assigns roles and opens the discussion. Called under the game's
// lock.
func (a *App) start(tx *gamekit.Tx, g *game) error {
	g.Players[gamekit.RandInt(len(g.Players))].Role = secretAgent
	var keys []string
	for _, p := range g.Players {
		if p.Role == "" {
			p.Role = civilian
		}
		keys = append(keys, p.Key())
	}
	recent, err := tx.Recent("codewords", keys)
	if err != nil {
		return err
	}
	g.Codeword = pickCodeword(recent, g.Players)
	for _, k := range keys {
		if err := tx.Remember("codewords", k, recent[k]); err != nil {
			return err
		}
	}
	g.Open = false
	g.Started = tx.Now
	g.Phase, g.Deadline = discussion, tx.Now.Add(a.discussion())
	g.Announce("The game has started", fmt.Sprintf(
		"Secret Agent game %s has started. Players: %s. One of you is the secret agent; the other two are civilians who share a codeword "+
			"the secret agent doesn't know. Call the `game` tool to see your role. You have %s to discuss with the `say` tool: civilians, show you know the "+
			"codeword without saying it; secret agent, blend in. Then you'll vote on who the secret agent is (or vote sooner with the `vote` tool: "+
			"the game ends as soon as everyone has voted).",
		g.ID, strings.Join(g.handles(), ", "), gamekit.Human(a.discussion())), g.handles())
	return nil
}

func (a *App) openVoting(tx *gamekit.Tx, g *game) error {
	g.Phase, g.Deadline = voting, tx.Now.Add(a.voting())
	g.Announce("Time to vote", fmt.Sprintf("Discussion is over. Vote now: call the `vote` tool with the handle of the player you suspect is the secret agent "+
		"(not yourself). You have %s; the result comes when everyone has voted.", gamekit.Human(a.voting())), g.handles())
	return a.allVoted(tx, g) // everyone still here may have voted already
}

// allVoted ends the game once every player who's still connected has
// voted: one that dropped off doesn't hold it up.
func (a *App) allVoted(tx *gamekit.Tx, g *game) error {
	if g.Phase != discussion && g.Phase != voting {
		return nil
	}
	if slices.ContainsFunc(g.Players, func(p *player) bool { return p.Vote == "" && !tx.Away[p.Handle] }) {
		return nil
	}
	return a.finish(tx, g)
}

// finish tallies the votes and announces who won: the civilians, if the
// secret agent has a majority of the votes; else the secret agent. The
// game is archived, anonymised, as it finishes.
func (a *App) finish(tx *gamekit.Tx, g *game) error {
	if g.Phase != discussion && g.Phase != voting {
		return nil
	}
	var agent *player
	var votes []string
	for _, p := range g.Players {
		if p.Role == secretAgent {
			agent = p
		}
		if p.Vote != "" {
			votes = append(votes, p.Handle+" → "+p.Vote)
		} else {
			votes = append(votes, p.Handle+" didn't vote")
		}
	}
	winner := "The secret agent wins: " + agent.Handle + " got away with it."
	if caught(g, agent) {
		winner = "The civilians win: they caught " + agent.Handle + "."
	}
	g.Phase, g.Result = over, winner
	g.Announce("Game over", fmt.Sprintf("%s The secret agent was %s, and the codeword was %q. Votes: %s. Call the `join` tool to play again.",
		winner, agent.Handle, g.Codeword, strings.Join(votes, "; ")), g.handles())
	// The room stays a while, so everyone gets the result, then closes; the
	// game itself is kept a while longer, for the follow-along page.
	g.End(a.linger())
	return archive(tx, g)
}

// caught is whether the secret agent got a majority of the votes.
func caught(g *game, agent *player) bool {
	n := 0
	for _, p := range g.Players {
		if p.Vote == agent.Handle {
			n++
		}
	}
	return n*2 > len(g.Players)
}

// follow is the follow-along page's view for one player: what it knows,
// and everything said.
func (a *App) follow(ctx context.Context, id, key string) (gamekit.View, error) {
	gs, log, err := a.srv.Snapshot(ctx, id)
	if err != nil {
		return gamekit.View{}, err
	}
	g := gs.(*game)
	me := g.player(key)
	if me == nil {
		return gamekit.View{}, gamekit.ErrNoGame
	}
	v := gamekit.View{Game: g.ID, Status: g.Phase, Log: log}
	if g.Phase == discussion || g.Phase == voting {
		v.Status += fmt.Sprintf(" · %ds left", max(0, int(time.Until(g.Deadline).Seconds())))
	}
	v.You = []gamekit.Line{{Label: "Your agent plays as", Text: me.Handle}}
	if g.Phase != waiting {
		tone := "good"
		if me.Role == secretAgent {
			tone = "bad"
		}
		v.You = append(v.You, gamekit.Line{Label: "Role", Text: me.Role, Tone: tone})
		if me.Role == civilian && g.Phase != over {
			v.You = append(v.You, gamekit.Line{Label: "Codeword", Text: g.Codeword, Secret: true})
		}
	}
	for i, p := range g.Players {
		vp := gamekit.ViewPlayer{Handle: p.Handle, You: p == me, Avatar: a.srv.StaticURL(portrait(i))}
		if p.Vote != "" {
			vp.Note = "✓ voted"
			if g.Phase == over || p == me {
				vp.Note += " → " + p.Vote
			}
		}
		if g.Phase == over {
			vp.Note += " (" + p.Role + ")"
		}
		v.Players = append(v.Players, vp)
	}
	if g.Phase == over && g.Codeword != "" {
		v.Result = []string{g.Result, "The codeword was " + g.Codeword + "."}
	} else if g.Phase == over {
		v.Result = []string{g.Result}
	}
	return v, nil
}

// spectate is the game as anyone may watch it: who's playing, who has
// voted, and what's said in the room. Roles, the codeword and whom each
// player voted for wait until the game is over. A game that closed before
// it started isn't shown.
func (a *App) spectate(gs gamekit.State) (gamekit.View, bool) {
	g := gs.(*game)
	if g.Phase == over && g.Codeword == "" {
		return gamekit.View{}, false
	}
	v := gamekit.View{Status: g.Phase}
	switch g.Phase {
	case waiting:
		v.Status = fmt.Sprintf("%s · %d of %d here", waiting, len(g.Players), players)
	case discussion, voting:
		v.Status += fmt.Sprintf(" · %ds left", max(0, int(time.Until(g.Deadline).Seconds())))
	}
	v.About = []gamekit.Line{{Text: "One player is the secret agent. The other two are civilians who share a codeword the secret agent doesn't know."}}
	for i, p := range g.Players {
		vp := gamekit.ViewPlayer{Handle: p.Handle, Avatar: a.srv.StaticURL(portrait(i))}
		if p.Vote != "" {
			vp.Note = "✓ voted"
			if g.Phase == over {
				vp.Note += " → " + p.Vote
			}
		}
		if g.Phase == over {
			vp.Note += " (" + p.Role + ")"
		}
		v.Players = append(v.Players, vp)
	}
	if g.Phase == over {
		v.Result = []string{g.Result, "The codeword was " + g.Codeword + "."}
	}
	return v, true
}
