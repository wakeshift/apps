package sketch

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// A room's life: it waits for its first players, then plays round after
// round with no end (a drawer, its word, strokes announced to the
// guessers, hints, the first right guess or the clock, a short pause),
// the drawer rotating through whoever's in the room. Players join and
// leave whenever they like. With too few left the room waits for more,
// and it closes only if nobody comes, or comes back, in time. Everything
// here runs under the game's lock.

// step is the game's deadline passing.
func (a *App) step(tx *gamekit.Tx, g *game) error {
	defer g.changed()
	switch g.Phase {
	case waiting:
		if len(g.active()) >= g.playing() {
			return a.nextRound(tx, g)
		}
		if !g.Until.IsZero() && tx.Now.Before(g.Until) {
			// A check while it waits: a room whose players are all gone
			// closes now, rather than taking newcomers.
			gone, err := a.deserted(tx, g)
			if err != nil {
				return err
			}
			if gone {
				return a.closeDeserted(tx, g)
			}
			g.Deadline = minTime(g.Until, tx.Now.Add(waitCheck))
			return nil
		}
		why := "Not enough players joined."
		if g.Round > 0 {
			why = "Not enough players came back."
		}
		return a.closeRoom(tx, g, fmt.Sprintf("%s in %s, so this room is closed.", strings.TrimSuffix(why, "."), gamekit.Human(a.wait())))
	case drawing:
		return a.drawingStep(tx, g)
	case reveal:
		return a.nextRound(tx, g)
	}
	return nil
}

// seated runs after a player sits down. In a waiting room, eight start
// play at once, and enough to play start a countdown (the lobby's, for a
// new room's first round; the pause between rounds, for one coming back).
// In a room that's playing, the joiner guesses from now on and draws when
// the rotation comes to it.
func (a *App) seated(tx *gamekit.Tx, g *game, p gamekit.Player) error {
	// Matchmaking counts seats, and a player who left has none: this only
	// guards against a room that's somehow full.
	if old := g.player(p.Key()); (old == nil || old.Left) && len(g.active()) >= maxPlayers {
		return errors.New("this room is full; call the `join` tool again")
	}
	g.seat(p)
	g.changed()
	if err := touch(tx.Ctx, tx, g.ID, p.Key(), tx.Now); err != nil {
		return err
	}
	switch g.Phase {
	case waiting:
		n := len(g.active())
		if n >= maxPlayers {
			return a.nextRound(tx, g)
		}
		if n < g.playing() {
			return nil
		}
		wait := a.lobby()
		if g.Round > 0 {
			wait = a.pause()
		}
		if at := tx.Now.Add(wait); g.Deadline.IsZero() || g.Deadline.After(at) {
			g.Deadline = at
			if g.Round == 0 {
				g.Announce("Starting soon", fmt.Sprintf("%s are here (%s). The first round starts in %s; more can join at any time, up to %d.",
					plural(n, "player", "players"), strings.Join(g.handles(), ", "), gamekit.Human(wait), maxPlayers), nil)
			} else {
				g.Announce("Back on", fmt.Sprintf("%s joined: the next round starts in %s.", p.Handle, gamekit.Human(wait)), nil)
			}
		}
	case drawing, reveal:
		g.Announce("A player joined", fmt.Sprintf("%s joined the room. It guesses from now on, and draws when its turn comes round.", p.Handle), nil)
	}
	return nil
}

// nextRound starts the next drawer's round, or has the room wait for
// players if too few are left.
func (a *App) nextRound(tx *gamekit.Tx, g *game) error {
	if err := a.checkAway(tx, g); err != nil {
		return err
	}
	g.prune()
	gone, err := a.deserted(tx, g)
	if err != nil {
		return err
	}
	if gone {
		return a.closeDeserted(tx, g)
	}
	if len(g.active()) < g.playing() {
		return a.idle(tx, g)
	}
	if g.Round == 0 {
		gamekit.Shuffle(g.Order)
		g.Turn = -1
		g.Announce("The room is playing", fmt.Sprintf("Sketch Stadium room %s is playing, with %s. Rounds go on without end: the drawer rotates through everyone here, "+
			"and players can join or leave at any time. The drawer gets a secret word and has %s to start drawing it, or the turn passes to the next drawer; the round then lasts %s from its first drawing. Everyone else looks with the `canvas` tool and guesses with `guess`. "+
			"The first right guess scores for the guesser and the drawer: 1, 2 or 3 points by the word's difficulty. Scores count the last %d rounds.",
			g.ID, strings.Join(g.handles(), ", "), gamekit.Human(a.grace()), gamekit.Human(a.round()), scoreWindow), g.handles())
	}
	next := g.nextDrawer()
	if next < 0 { // can't happen: prune puts everyone here in the rotation
		return a.idle(tx, g)
	}
	drawer := g.player(g.Order[next])
	w, err := a.pickWord(tx, g)
	if err != nil {
		return err
	}
	g.Turn, g.Round = next, g.Round+1
	g.Phase, g.Word, g.Tier, g.Drawer = drawing, w.Word, w.Tier, drawer.Key()
	// Until the drawer draws, the round ends at its deadline to start; its
	// first drawing starts the round's clock (drew).
	g.RoundStart, g.DrawnAt, g.RoundEnd = tx.Now, time.Time{}, tx.Now.Add(a.grace())
	g.Strokes, g.Drawn, g.Hints, g.Announced, g.LastFlush, g.FlushAt, g.Guesses, g.Outcome = nil, false, 0, 0, time.Time{}, time.Time{}, nil, ""
	g.Shown, g.RoundOver, g.Until, g.Draws, g.HintTimes = time.Time{}, time.Time{}, time.Time{}, nil, nil
	for _, p := range g.Players {
		p.Guessed, p.LastGuess = 0, time.Time{}
	}
	g.Announce(fmt.Sprintf("Round %d: %s draws", g.Round, drawer.Handle), fmt.Sprintf("Round %d: %s is drawing a %s word, worth %s. %s: call the `game` tool for your word, "+
		"and start drawing it with `draw` within %s, or the turn passes to the next drawer. The round lasts %s from the first drawing. "+
		"Everyone else: you'll be woken as the drawing arrives; look with the `canvas` tool and `guess` the word.", g.Round, drawer.Handle, w.Tier,
		plural(tierPoints(w.Tier), "point", "points"), drawer.Handle, gamekit.Human(a.grace()), gamekit.Human(a.round())), g.handles())
	if err := a.queueTurn(tx, g, drawer); err != nil {
		return err
	}
	a.schedule(g)
	return nil
}

// idle has the room wait for players: too few are left to play. It plays
// again as soon as enough are back, and closes if they aren't in time.
func (a *App) idle(tx *gamekit.Tx, g *game) error {
	g.Phase, g.Word, g.Tier, g.Drawer, g.Outcome = waiting, "", "", "", ""
	g.Strokes, g.Drawn, g.Hints, g.Announced, g.LastFlush, g.FlushAt, g.Guesses = nil, false, 0, 0, time.Time{}, time.Time{}, nil
	g.Shown, g.RoundOver, g.Draws, g.HintTimes, g.DrawnAt = time.Time{}, time.Time{}, nil, nil, time.Time{}
	a.waitForPlayers(tx, g)
	g.changed()
	here := "Nobody is here"
	if n := len(g.active()); n > 0 {
		here = "Only " + plural(n, "player is", "players are") + " here"
	}
	g.Announce("Waiting for players", fmt.Sprintf("%s, so the room waits. Play resumes as soon as %d are here (anyone can join with `join`); "+
		"if nobody comes in %s, the room closes.", here, g.playing(), gamekit.Human(a.wait())), g.handles())
	return nil
}

// waitForPlayers has a room wait for enough players: it closes after
// wait if they don't come. One that's played checks every waitCheck that
// someone in it is still there (step), and closes sooner if nobody is.
func (a *App) waitForPlayers(tx *gamekit.Tx, g *game) {
	g.Until, g.Deadline = tx.Now.Add(a.wait()), tx.Now.Add(a.wait())
	if g.Round > 0 {
		g.Deadline = minTime(g.Until, tx.Now.Add(waitCheck))
	}
}

// checkAway runs as the next round starts: it counts who was away for the
// round just played, and takes out a player away awayRounds in a row, as
// if it had left. Being connected isn't enough to be there: an agent whose
// service restarted can look connected for hours. So:
//
//   - a drawer that drew nothing was away;
//   - a guesser that did nothing all round (no tool call as its player, no
//     guess) was away if the drawing was up long enough to answer
//     (respondWithin); with nothing to answer, it was away only if nothing
//     delivers to it (tx.Away), and otherwise it's as it was;
//   - anything a player does in a round counts it there, and its count
//     starts again.
func (a *App) checkAway(tx *gamekit.Tx, g *game) error {
	if g.Drawer == "" {
		return nil // no round played since the room last waited
	}
	last, err := seen(tx, g)
	if err != nil {
		return err
	}
	over := g.RoundOver
	if over.Before(g.RoundStart) {
		over = tx.Now
	}
	answerable := !g.Shown.IsZero() && over.Sub(g.Shown) >= respondWithin
	var out []string
	for _, p := range g.active() {
		drawer := p.Key() == g.Drawer
		acted := !last[p.Key()].Before(g.RoundStart) || p.Guessed > 0
		switch {
		case drawer && g.Drawn, !drawer && acted:
			p.Missed = 0
			continue
		case drawer, answerable, tx.Away[p.Handle]:
			p.Missed++
		default:
			continue
		}
		if p.Missed >= awayRounds {
			why := fmt.Sprintf("You were taken out of Sketch Stadium room %s: away for %s in a row (nothing drawn on your turn, no guess or tool call while drawings were up). "+
				"The room plays on without you. Call the `join` tool to play again.", g.ID, plural(awayRounds, "round", "rounds"))
			if err := a.remove(tx, g, p, why); err != nil {
				return err
			}
			out = append(out, p.Handle)
		}
	}
	if len(out) > 0 {
		g.changed()
		g.Announce(fmt.Sprintf("%s left the room", strings.Join(out, " and ")), fmt.Sprintf("%s left the room: away for %s in a row. The room plays on without %s; `join` to play again.",
			strings.Join(out, " and "), plural(awayRounds, "round", "rounds"), map[bool]string{true: "it", false: "them"}[len(out) == 1]), nil)
	}
	return nil
}

// present reports whether p is still playing: it's done something in the
// last idleClose, or it's missed no round and something delivers to it.
func present(p *player, last map[string]time.Time, now time.Time, away map[string]bool) bool {
	return now.Sub(last[p.Key()]) < idleClose || p.Missed == 0 && !away[p.Handle]
}

// deserted reports whether a room that's played has players but none
// still playing: a room of agents that have gone, which mustn't take
// newcomers or play on for nobody. (An empty one waits for players.)
func (a *App) deserted(tx *gamekit.Tx, g *game) (bool, error) {
	ps := g.active()
	if g.Round == 0 || len(ps) == 0 {
		return false, nil
	}
	last, err := seen(tx, g)
	if err != nil {
		return false, err
	}
	for _, p := range ps {
		if present(p, last, tx.Now, tx.Away) {
			return false, nil
		}
	}
	return true, nil
}

// closeDeserted closes a room nobody in it is playing any more.
func (a *App) closeDeserted(tx *gamekit.Tx, g *game) error {
	g.changed()
	return a.closeRoom(tx, g, fmt.Sprintf("Nobody has played in this room for %s, so it's closed.", gamekit.Human(idleClose)))
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// remove takes a player out of the room: its seat goes (it can join again),
// its watch link stops working, and once this commits its seat's
// subscription is ended, unless it's seated in another game (gamekit's
// Tx.Remove). Leaving and being taken out are the same but for why: a
// player taken out is told why, and how to play again, in a last update to
// it alone before its subscription ends; one that left (why "") knows.
func (a *App) remove(tx *gamekit.Tx, g *game, p *player, why string) error {
	p.Left = true
	subject := ""
	if why != "" {
		subject = "You left the room"
	}
	if _, err := tx.Remove(g.ID, p.Key(), subject, why); err != nil {
		return err
	}
	if _, err := tx.Exec(tx.Ctx, `DELETE FROM sketch_seen WHERE game_id = $1 AND player = $2`, g.ID, p.Key()); err != nil {
		return err
	}
	_, err := tx.Exec(tx.Ctx, `DELETE FROM sketch_watch WHERE game_id = $1 AND player = $2`, g.ID, p.Key())
	return err
}

// removal is a removed seat's subscription, to end once the removal
// committed: what remove queued before gamekit's Tx.Remove did it. Rows
// queued then are still sent (store.go).
type removal struct {
	Subscription string `json:"subscription"`
	User         string `json:"user"`
}

// pickWord is a word for the next round, of a tier by the room's odds,
// that neither the room nor anyone in it has had lately.
func (a *App) pickWord(tx *gamekit.Tx, g *game) (word, error) {
	var keys []string
	for _, p := range g.active() {
		keys = append(keys, p.Key())
	}
	recent, err := tx.Recent("words", keys)
	if err != nil {
		return word{}, err
	}
	seen := map[string]bool{}
	for _, w := range g.Recent {
		seen[w] = true
	}
	for _, ws := range recent {
		for _, w := range ws {
			seen[w] = true
		}
	}
	odds := a.tierOdds(g)
	roll, tier := gamekit.RandInt(10), hard
	for _, t := range odds {
		if roll < t.n {
			tier = t.tier
			break
		}
		roll -= t.n
	}
	var pool, fresh, anyFresh []word
	for _, w := range words {
		if !seen[w.Word] {
			anyFresh = append(anyFresh, w)
		}
		if w.Tier == tier {
			pool = append(pool, w)
			if !seen[w.Word] {
				fresh = append(fresh, w)
			}
		}
	}
	switch {
	case len(fresh) > 0:
		pool = fresh
	case len(anyFresh) > 0:
		pool = anyFresh
	}
	w := pool[gamekit.RandInt(len(pool))]
	g.Recent = remembered(w.Word, g.Recent, roomWords)
	for _, k := range keys {
		if err := tx.Remember("words", k, remembered(w.Word, recent[k], recentWords)); err != nil {
			return word{}, err
		}
	}
	return w, nil
}

// remembered is had with w in front, at most n long.
func remembered(w string, had []string, n int) []string {
	out := append([]string{w}, had...)
	return slices.Clone(out[:min(len(out), n)])
}

// paceRounds is how many of a room's latest rounds its odds go by.
const paceRounds = 6

// tierOdds is how likely each tier is in g's next round: mostly medium and
// hard words; harder still for a room that's been guessing them quickly,
// and a little easier for one where the clock keeps running out.
func (a *App) tierOdds(g *game) []tierOdd {
	var timed, fast, slow int
	quick := a.round() / 4
	for i := len(g.Rounds) - 1; i >= 0 && timed < paceRounds; i-- {
		switch r := g.Rounds[i]; r.Outcome {
		case "guessed":
			timed++
			if time.Duration(r.Seconds)*time.Second <= quick {
				fast++
			}
		case "timeout":
			timed++
			slow++
		}
	}
	switch {
	case timed >= 3 && fast*2 > timed:
		return fastOdds
	case timed >= 3 && slow*2 > timed:
		return slowOdds
	}
	return tierOdds
}

// schedule sets the game's deadline: the soonest of what's due in the
// round (the drawer's start, strokes to announce, a hint, the end).
func (a *App) schedule(g *game) {
	if g.Phase != drawing {
		return
	}
	d := g.RoundEnd
	soon := func(t time.Time) {
		if !t.IsZero() && t.Before(d) {
			d = t
		}
	}
	if !g.Drawn {
		soon(g.RoundStart.Add(a.grace()))
	}
	soon(g.FlushAt)
	if g.Drawn && g.Hints < 2 {
		soon(a.hintAt(g, g.Hints+1))
	}
	g.Deadline = d
}

// hintAt is when hint n comes, once the drawer has drawn: the length
// halfway through the round's clock, the first letter three quarters of
// the way (30 s and 45 s after the first drawing, by default).
func (a *App) hintAt(g *game, n int) time.Time {
	frac := map[int]float64{1: 0.5, 2: 0.75}[n]
	start := g.clockStart()
	return start.Add(time.Duration(float64(g.RoundEnd.Sub(start)) * frac))
}

// drawingStep is a deadline in a round: whatever's due, in order.
func (a *App) drawingStep(tx *gamekit.Tx, g *game) error {
	now := tx.Now
	d := g.drawer()
	if d == nil || d.Left {
		return a.endRound(tx, g, nil, "void", fmt.Sprintf("The drawer left, so round %d is void. The word was %q.", g.Round, g.Word))
	}
	if !g.Drawn && !now.Before(g.RoundStart.Add(a.grace())) {
		// The turn passes: the next drawer starts at once, and this one was
		// away for the round (checkAway).
		last, err := seen(tx, g)
		if err != nil {
			return err
		}
		why := "didn't start drawing"
		if last[d.Key()].Before(g.RoundStart) {
			why = "didn't respond" // not even for its word
		}
		if err := a.endRound(tx, g, nil, "forfeit", fmt.Sprintf("%s %s within %s, so round %d is forfeited and the turn passes to the next drawer. The word was %q. Nobody scores.",
			d.Handle, why, gamekit.Human(a.grace()), g.Round, g.Word)); err != nil {
			return err
		}
		return a.nextRound(tx, g)
	}
	if !now.Before(g.RoundEnd) {
		return a.endRound(tx, g, nil, "timeout", fmt.Sprintf("Time's up! Nobody guessed it: the word was %q. Nobody scores this round.", g.Word))
	}
	if !g.FlushAt.IsZero() && !now.Before(g.FlushAt) {
		a.flush(tx, g)
	}
	for g.Drawn && g.Hints < 2 && !now.Before(a.hintAt(g, g.Hints+1)) {
		g.Hints++
		g.HintTimes = append(g.HintTimes, now)
		text := fmt.Sprintf("Hint: the word has %s: %s", g.letterCount(), g.hint())
		if g.Hints == 2 {
			text = fmt.Sprintf("Hint: it starts with %q: %s", strings.ToUpper(g.Word[:1]), g.hint())
		}
		g.Announce("Hint", fmt.Sprintf("%s. %ds left. Guess with `guess`.", text, secondsLeft(now, g.RoundEnd)), nil)
	}
	a.schedule(g)
	return nil
}

// drew adds a drawer's strokes, and announces them to the guessers now if
// the last announcement was long enough ago, else when it will have been.
func (a *App) drew(tx *gamekit.Tx, g *game, strokes []stroke) {
	g.Strokes = append(g.Strokes, strokes...)
	g.Draws = append(g.Draws, drawCallAt{At: tx.Now, Marks: len(strokes)})
	if !g.Drawn {
		// The first drawing starts the round's clock.
		g.DrawnAt, g.RoundEnd = tx.Now, tx.Now.Add(a.round())
	}
	g.Drawn = true
	g.changed()
	if next := g.LastFlush.Add(a.coalesce()); g.LastFlush.IsZero() || !tx.Now.Before(next) {
		a.flush(tx, g)
	} else if g.FlushAt.IsZero() {
		g.FlushAt = next
	}
	a.schedule(g)
}

// flush announces the strokes drawn since the last announcement.
func (a *App) flush(tx *gamekit.Tx, g *game) {
	g.FlushAt = time.Time{}
	if g.Announced >= len(g.Strokes) {
		return
	}
	d := g.drawer()
	fresh := g.Strokes[g.Announced:]
	what := "strokes"
	if _, sh, _ := marks(fresh); sh > 0 {
		what = "marks" // a shape's points are too many to list: the image shows it
	}
	text := fmt.Sprintf("%s drew %s (%s %d–%d; [x, y] from 0 to 100, top left first):\n%s\n\nLook at the whole drawing with the `canvas` tool, then `guess` the word. %ds left.",
		d.Handle, marksSaid(fresh), what, g.Announced+1, len(g.Strokes), strokeText(fresh, g.Announced+1), secondsLeft(tx.Now, g.RoundEnd))
	if h := g.hint(); h != "" {
		text += " Hint: " + h
	}
	g.Announce(fmt.Sprintf("%s drew %s", d.Handle, marksSaid(fresh)), text, guessers(g))
	g.Announced, g.LastFlush = len(g.Strokes), tx.Now
	if g.Shown.IsZero() {
		g.Shown = tx.Now
	}
}

func guessers(g *game) []string {
	var out []string
	for _, p := range g.active() {
		if p.Key() != g.Drawer {
			out = append(out, p.Handle)
		}
	}
	return out
}

// endRound ends the round: winner guessed it (nil: nobody did), or it
// ended some other way (outcome). The word's out once it ends, the round
// joins the room's last scoreWindow, and everyone's score is worked out
// again: the oldest round's points drop off.
func (a *App) endRound(tx *gamekit.Tx, g *game, winner *player, outcome, text string) error {
	s := summary{Round: g.Round, Word: g.Word, Outcome: outcome, Tier: g.Tier}
	if d := g.drawer(); d != nil {
		s.Drawer, s.DrawerKey = d.Handle, d.Key()
	}
	if winner != nil {
		s.Winner, s.WinnerKey, s.Points = winner.Handle, winner.Key(), tierPoints(g.Tier)
		s.Seconds = int(tx.Now.Sub(g.clockStart()).Seconds()) // from the first drawing
	}
	g.Rounds = append(g.Rounds, s)
	if n := len(g.Rounds); n > scoreWindow {
		g.Rounds = slices.Clone(g.Rounds[n-scoreWindow:])
	}
	g.rescore()
	g.Outcome, g.RoundOver = outcome, tx.Now
	g.Phase, g.Deadline, g.FlushAt = reveal, tx.Now.Add(a.pause()), time.Time{}
	g.changed()
	switch {
	case outcome == "forfeit":
	case len(g.active()) < keepPlayers:
		text += " Too few players are left to go on: the room waits for more."
	default:
		text += fmt.Sprintf(" Next round in %s.", gamekit.Human(a.pause()))
	}
	g.Announce(fmt.Sprintf("Round %d over", g.Round), text+" "+scoreLine(g), g.handles())
	if err := a.archive(tx, g); err != nil {
		return err
	}
	return a.recordBest(tx, g)
}

// scoreLine is the scores: "Scores (last 20 rounds): heron 4, otter 2."
func scoreLine(g *game) string {
	var parts []string
	for _, p := range g.standings() {
		if !p.Left {
			parts = append(parts, fmt.Sprintf("%s %d", p.Handle, p.Score))
		}
	}
	return fmt.Sprintf("Scores (last %d rounds): %s.", scoreWindow, strings.Join(parts, ", "))
}

// rightGuess is p guessing the word (with text): the word's points each
// for it and the drawer.
func (a *App) rightGuess(tx *gamekit.Tx, g *game, p *player, text string, owner bool) error {
	d := g.drawer()
	// The round ends with it, in this change: its text first shows with
	// the word.
	g.Guesses = append(g.Guesses, guess{By: p.Handle, Text: text, Right: true, Owner: owner, At: tx.Now})
	by := p.Handle
	if owner {
		by = p.Handle + " (guessed by its owner)"
	}
	dh := "the drawer"
	if d != nil {
		dh = d.Handle
	}
	return a.endRound(tx, g, p, "guessed", fmt.Sprintf("%s guessed it in %s: the word was %q! %s each for %s and %s.",
		by, gamekit.Human(tx.Now.Sub(g.clockStart())), g.Word, plural(tierPoints(g.Tier), "point", "points"), p.Handle, dh))
}

// closeRoom closes the room: nobody came, or came back, in time.
func (a *App) closeRoom(tx *gamekit.Tx, g *game, why string) error {
	g.Phase, g.Word, g.Drawer = over, "", ""
	g.Strokes, g.Guesses = nil, nil
	g.Result = []string{why}
	if len(g.active()) > 0 && g.Round > 0 {
		g.Result = append(g.Result, scoreLine(g))
	}
	g.Announce("Room closed", strings.Join(g.Result, " ")+" Call the `join` tool to play again.", g.handles())
	g.End(a.linger())
	_, err := tx.Exec(tx.Ctx, `DELETE FROM sketch_watch WHERE game_id = $1`, g.ID)
	return err
}

// leave takes p out of the room at its own request. The room goes on
// without it: a round it was drawing is void, and with too few left the
// room waits for players.
func (a *App) leave(tx *gamekit.Tx, g *game, p *player) error {
	if err := a.remove(tx, g, p, ""); err != nil {
		return err
	}
	g.changed()
	switch {
	case g.Phase == over:
		return nil
	case g.Phase == waiting:
		g.prune()
		if n := len(g.active()); n < g.playing() && n+1 >= g.playing() {
			// Too few now: the countdown stops, and the room waits.
			a.waitForPlayers(tx, g)
		}
	case g.isDrawing(p):
		return a.endRound(tx, g, nil, "void", fmt.Sprintf("%s left while drawing, so round %d is void. The word was %q.", p.Handle, g.Round, g.Word))
	case g.Phase == drawing && len(g.active()) < keepPlayers:
		return a.endRound(tx, g, nil, "void", fmt.Sprintf("%s left, so round %d is void. The word was %q.", p.Handle, g.Round, g.Word))
	case g.Phase == reveal:
		// Between rounds: off the scoreboard now, and with too few left the
		// room waits at once.
		g.prune()
		if len(g.active()) < keepPlayers {
			g.Announce("A player left", p.Handle+" left the room.", nil)
			return a.idle(tx, g)
		}
	}
	g.Announce("A player left", p.Handle+" left the room.", nil)
	return nil
}
