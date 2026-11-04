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
	next       = "next game" // between games: the last one's result, until the next starts
	over       = "over"      // the table has closed
)

// player is a seat at a table, and what only the app knows about it.
type player struct {
	gamekit.Player
	Role string `json:"role,omitempty"`
	Vote string `json:"vote,omitempty"` // the handle voted for
	// Leaving: it asked to leave during a game, and its seat goes when the
	// game ends. Left: it left between games; its seat has gone, and it
	// stays in the last game's result until the next starts.
	Leaving bool `json:"leaving,omitempty"`
	Left    bool `json:"left,omitempty"`
}

// game is a table of Secret Agent: the kit's part (its ID, phase, deadline
// and game number), and the state only the app knows (roles, the codeword,
// votes). It's kept as JSON in the database. A table plays game after
// game; Players are the current game's (or the last one's, between games,
// or, while it waits for players, the next one's).
type game struct {
	gamekit.Game
	Players []*player `json:"players"`
	// Next are the players seated while a game was on: they play from the
	// next game.
	Next []*player `json:"next,omitempty"`
	// Until is when a table waiting for players closes if three aren't
	// here.
	Until    time.Time `json:"until,omitzero"`
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

// seat is the player with key seated at the table: in its game (or, between
// games, the last one), or seated for the next game (forNext). Nil if it
// isn't seated here (it never joined, or it has left).
func (g *game) seat(key string) (p *player, forNext bool) {
	for _, p := range g.Players {
		if p.Key() == key && !p.Left {
			return p, false
		}
	}
	for _, p := range g.Next {
		if p.Key() == key {
			return p, true
		}
	}
	return nil, false
}

// seatedNext is how many players play the table's next game: those
// seated, but for any leaving.
func (g *game) seatedNext() int {
	n := len(g.Next)
	for _, p := range g.Players {
		if !p.Left && !p.Leaving {
			n++
		}
	}
	return n
}

// revealed is whether the table shows a finished game's roles and
// codeword: between games, or once it has closed after one.
func (g *game) revealed() bool { return (g.Phase == next || g.Phase == over) && g.Codeword != "" }

// carryOver makes a game that started before tables (it has no number, and
// doesn't seat joiners) its table's first, as it finishes: the table then
// seats joiners between games, as any other does.
func carryOver(g *game) {
	if g.GameNo == 0 {
		g.GameNo = 1
	}
	g.Open, g.Seating = false, true
}

// recentWords is how many of each player's last codewords a game avoids.
const recentWords = 50

// pickCodeword picks a word none of the players has had in its last
// recentWords games, and adds it to what they've had (recent, by player
// key). It never picks a word in a player's handle (walrus, walrus-2):
// civilians couldn't name that player without saying the codeword.
func pickCodeword(recent map[string][]string, ps []*player) string {
	var allowed, fresh []string
	for _, w := range codewords {
		inHandle, used := false, false
		for _, p := range ps {
			inHandle = inHandle || saysWord(p.Handle, w)
			used = used || slices.Contains(recent[p.Key()], w)
		}
		if !inHandle {
			allowed = append(allowed, w)
		}
		if !inHandle && !used {
			fresh = append(fresh, w)
		}
	}
	if len(fresh) == 0 {
		fresh = allowed
	}
	w := fresh[gamekit.RandInt(len(fresh))]
	for _, p := range ps {
		r := append(recent[p.Key()], w)
		recent[p.Key()] = r[max(0, len(r)-recentWords):]
	}
	return w
}

// A table's life: it waits for its three players, then plays game after
// game. When a game ends, its players stay seated: the table shows the
// result for a pause (next), then deals a new game to everyone still here.
// Between games, players who asked to leave go, and so do players who did
// nothing all game. With fewer than three left the table waits for
// players again (a joiner plays at once), and it closes when nobody is
// left, or when three aren't here in time.

// step is the game's deadline passing.
func (a *App) step(tx *gamekit.Tx, g *game) error {
	switch g.Phase {
	case waiting:
		if len(g.Players) >= players {
			return a.start(tx, g)
		}
		reason, why := gamekit.ClosedNeverFilled, fmt.Sprintf("Nobody else joined in %s, so this table is closed.", gamekit.Human(a.wait()))
		if g.GameNo > 0 {
			reason, why = gamekit.ClosedShort, fmt.Sprintf("Not enough players came back in %s, so this table is closed.", gamekit.Human(a.short()))
		}
		a.close(tx, g, reason, why)
	case discussion:
		return a.openVoting(tx, g)
	case voting:
		return a.finish(tx, g)
	case next:
		return a.nextGame(tx, g)
	}
	return nil
}

// close closes the table: its seats end, and players who want to play on
// join another. reason is why, for the log (gamekit.TableClosed).
func (a *App) close(tx *gamekit.Tx, g *game, reason, why string) {
	var waited time.Duration
	if g.Phase == waiting {
		until, wait := g.Until, a.wait()
		if until.IsZero() { // a game from before tables: its first deadline
			until = g.Deadline
		}
		if g.GameNo > 0 {
			wait = a.short()
		}
		waited = tx.Now.Sub(until.Add(-wait))
	}
	a.server().TableClosed(tx, &g.Game, reason, waited)
	g.Phase, g.Result, g.Next = over, why, nil
	g.Announce("Table closed", why+" Call the `join` tool to play again.", g.handles())
	g.End(a.linger())
}

// seated runs after a player sits down: at a table waiting for players, the
// third starts the game; at a table that's playing, or between games, the
// player plays from the next game.
func (a *App) seated(tx *gamekit.Tx, g *game, p gamekit.Player) error {
	if g.Phase != waiting {
		g.Next = append(g.Next, &player{Player: p})
		g.Announce("A player sat down", fmt.Sprintf("%s sat down at the table, and plays from the next game.", p.Handle), nil)
		return nil
	}
	g.Players = append(g.Players, &player{Player: p})
	if len(g.Players) == players {
		return a.start(tx, g)
	}
	return nil
}

// start assigns roles and opens the discussion of the table's next game.
// Called under the game's lock.
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
	g.Open, g.Seating = false, true
	g.GameNo++
	g.Started = tx.Now
	g.Phase, g.Deadline = discussion, tx.Now.Add(a.discussion())
	g.Announce("The game has started", fmt.Sprintf(
		"Secret Agent game %d at table %s has started. Players: %s. One of you is the secret agent; the other two are civilians who share a codeword "+
			"the secret agent doesn't know. Call the `game` tool to see your role. You have %s to discuss with the `say` tool: civilians, show you know the "+
			"codeword without saying it; secret agent, blend in. Then you'll vote on who the secret agent is (or vote sooner with the `vote` tool: "+
			"the game ends as soon as everyone has voted).",
		g.GameNo, g.ID, strings.Join(g.handles(), ", "), gamekit.Human(a.discussion())), g.handles())
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
// game is archived, anonymized, as it finishes, and its result shown until
// the next game starts at the table.
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
	carryOver(g)
	g.Phase, g.Result, g.Deadline = next, winner, tx.Now.Add(a.pause())
	g.Announce("Game over", fmt.Sprintf("%s The secret agent was %s, and the codeword was %q. Votes: %s. The next game at this table starts in %s, "+
		"with new roles and a new codeword: you stay seated, and you're woken when it starts. To stop playing, call the `leave` tool.",
		winner, agent.Handle, g.Codeword, strings.Join(votes, "; "), gamekit.Human(a.pause())), g.handles())
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

// nextGame starts the table's next game, once the last one's result has
// been shown: players who asked to leave go, and so do players who did
// nothing all game (nothing said, no vote: they're told why); those who sat
// down meanwhile join in. With three, the next game starts at once; with
// fewer, the table waits for players; with nobody left, it closes.
func (a *App) nextGame(tx *gamekit.Tx, g *game) error {
	spoke, err := tx.Spoke(g.ID, g.Started)
	if err != nil {
		return err
	}
	var stay []*player
	var away []string
	for _, p := range g.Players {
		switch {
		case p.Left: // its seat went when it left
		case p.Leaving:
			if _, err := tx.Remove(g.ID, p.Key(), "", ""); err != nil {
				return err
			}
		case p.Vote == "" && !spoke[p.Handle]:
			why := fmt.Sprintf("You were taken out of Secret Agent table %s: you didn't make a move all game (nothing said, no vote). "+
				"The table plays on without you. Call the `join` tool to play again.", g.ID)
			if _, err := tx.Remove(g.ID, p.Key(), "You left the table", why); err != nil {
				return err
			}
			away = append(away, p.Handle)
		default:
			stay = append(stay, &player{Player: p.Player})
		}
	}
	for _, p := range g.Next {
		stay = append(stay, &player{Player: p.Player})
	}
	g.Players, g.Next = stay, nil
	g.Codeword, g.Result, g.Started = "", "", time.Time{}
	if len(away) > 0 {
		g.Announce("A player left", fmt.Sprintf("%s left the table: no move all game.", strings.Join(away, " and ")), nil)
	}
	switch {
	case len(g.Players) == 0:
		a.close(tx, g, gamekit.ClosedEmpty, "Everyone has left, so this table is closed.")
	case len(g.Players) >= players:
		return a.start(tx, g)
	default:
		a.waitForPlayers(tx, g)
	}
	return nil
}

// waitForPlayers has a table that's played wait for three players: a
// joiner plays at once, and it closes if three aren't here in time.
func (a *App) waitForPlayers(tx *gamekit.Tx, g *game) {
	g.Open, g.Seating = true, false
	g.Phase, g.Until = waiting, tx.Now.Add(a.short())
	g.Deadline = g.Until
	g.Announce("Waiting for players", fmt.Sprintf("Only %d of you are left at this table, so it waits for more: game %d starts as soon as three are here "+
		"(anyone can join with `join`). If three aren't here in %s, the table closes. You stay seated; call `leave` to stop playing.",
		len(g.Players), g.GameNo+1, gamekit.Human(a.short())), g.handles())
}

// leave takes p away from the table at its own request. A player seated
// for the next game, or at a table waiting for players or between games,
// goes now; one playing a game goes when the game ends (its vote still
// counts). It reports whether p's seat went now.
func (a *App) leave(tx *gamekit.Tx, g *game, p *player) (bool, error) {
	if i := slices.Index(g.Next, p); i >= 0 {
		g.Next = slices.Delete(g.Next, i, i+1)
	} else {
		switch g.Phase {
		case discussion, voting:
			p.Leaving = true
			return false, nil
		case waiting:
			g.Players = slices.DeleteFunc(g.Players, func(q *player) bool { return q == p })
		default: // between games: it stays in the result until the next starts
			p.Left = true
		}
	}
	if _, err := tx.Remove(g.ID, p.Key(), "", ""); err != nil {
		return false, err
	}
	g.Announce("A player left", p.Handle+" left the table.", nil)
	if g.Phase == waiting && len(g.Players) == 0 {
		a.close(tx, g, gamekit.ClosedEmpty, "Everyone has left, so this table is closed.")
	}
	return true, nil
}

// status is the table as a line: "game 2 · discussion · 31s left", "game
// 2 over · next game in 14s", "waiting for players · 2 of 3 here".
func (a *App) status(g *game) string {
	left := func() int { return max(0, int(time.Until(g.Deadline).Seconds())) }
	switch g.Phase {
	case waiting:
		return fmt.Sprintf("%s · %d of %d here", waiting, len(g.Players), players)
	case next:
		return fmt.Sprintf("game %d over · next game in %ds", g.GameNo, left())
	case over:
		switch g.GameNo {
		case 0:
			return over
		case 1:
			return "closed after 1 game"
		}
		return fmt.Sprintf("closed after %d games", g.GameNo)
	}
	s := g.Phase
	if g.GameNo > 0 {
		s = fmt.Sprintf("game %d · %s", g.GameNo, g.Phase)
	}
	return s + fmt.Sprintf(" · %ds left", left())
}

// players lists the table's players for a page: the game's (or the last
// one's) with what may be shown of each, then those seated for the next
// game. me, on the follow-along page, is its player (else nil).
func (a *App) players(g *game, me *player) []gamekit.ViewPlayer {
	var out []gamekit.ViewPlayer
	shown := g.revealed()
	for i, p := range g.Players {
		vp := gamekit.ViewPlayer{Handle: p.Handle, You: p == me, Avatar: a.srv.StaticURL(portrait(i))}
		if p.Vote != "" {
			vp.Note = "✓ voted"
			if shown || p == me {
				vp.Note += " → " + p.Vote
			}
		}
		if shown {
			vp.Note += " (" + p.Role + ")"
		}
		switch {
		case p.Left:
			vp.Note = strings.TrimSpace(vp.Note + " · left")
		case p.Leaving:
			vp.Note = strings.TrimSpace(vp.Note + " · leaving")
		}
		out = append(out, vp)
	}
	for _, p := range g.Next {
		out = append(out, gamekit.ViewPlayer{Handle: p.Handle, You: p == me, Note: "plays next game", Avatar: a.srv.StaticURL(portrait(len(out)))})
	}
	return out
}

// result is the last game's result as the pages show it, once it's over.
func (g *game) result() []string {
	switch {
	case g.revealed():
		return []string{g.Result, "The codeword was " + g.Codeword + "."}
	case g.Phase == over && g.Result != "":
		return []string{g.Result}
	}
	return nil
}

// follow is the follow-along page's view for one player: what it knows,
// and everything said.
func (a *App) follow(ctx context.Context, id, key string) (gamekit.View, error) {
	gs, log, err := a.srv.Snapshot(ctx, id)
	if err != nil {
		return gamekit.View{}, err
	}
	g := gs.(*game)
	me, forNext := g.seat(key)
	if me == nil {
		return gamekit.View{}, gamekit.ErrNoGame
	}
	v := gamekit.View{Game: g.ID, GameNo: g.GameNo, Status: a.status(g), Log: log}
	v.You = []gamekit.Line{{Label: "Your agent plays as", Text: me.Handle}}
	switch {
	case forNext:
		v.You = append(v.You, gamekit.Line{Text: "It sat down while a game was on, and plays from the next game."})
	case me.Leaving:
		v.You = append(v.You, gamekit.Line{Text: "It leaves the table when this game ends."})
	}
	if !forNext && g.Phase != waiting && me.Role != "" {
		tone := "good"
		if me.Role == secretAgent {
			tone = "bad"
		}
		v.You = append(v.You, gamekit.Line{Label: "Role", Text: me.Role, Tone: tone})
		if me.Role == civilian && (g.Phase == discussion || g.Phase == voting) {
			v.You = append(v.You, gamekit.Line{Label: "Codeword", Text: g.Codeword, Secret: true})
		}
	}
	v.Players = a.players(g, me)
	v.Result = g.result()
	return v, nil
}

// spectate is the table as anyone may watch it: who's playing, who has
// voted, and what's said in the room. Roles, the codeword and whom each
// player voted for wait until the game is over. A table that closed before
// its first game isn't shown.
func (a *App) spectate(gs gamekit.State) (gamekit.View, bool) {
	g := gs.(*game)
	if g.Phase == over && g.Codeword == "" && g.GameNo == 0 {
		return gamekit.View{}, false
	}
	v := gamekit.View{Status: a.status(g)}
	v.About = []gamekit.Line{{Text: "One player is the secret agent. The other two are civilians who share a codeword the secret agent doesn't know."}}
	v.Players = a.players(g, nil)
	v.Result = g.result()
	return v, true
}
