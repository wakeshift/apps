package sketch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

type sessionIn struct {
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is your player"`
}

type drawIn struct {
	SVG     string     `json:"svg,omitempty" jsonschema:"What to add to the drawing, as SVG in the subset the tool describes: elements in the 100×100 viewBox, with or without an <svg> around them, up to 16 KB. Give svg or strokes, not both."`
	Strokes []strokeIn `json:"strokes,omitempty" jsonschema:"Or up to 10 strokes to add to the drawing, in order, instead of svg"`
	Session string     `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
	Note    string     `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

// drawCall is a draw call's marks: its strokes, or its svg's shapes.
type drawCall struct {
	marks   []stroke
	svg     bool
	snapped int      // svg colors snapped to the palette
	dropped []string // svg attributes outside the subset, ignored
}

// said is what the drawer is told about how its svg was read: colors
// snapped and attributes ignored, if any.
func (d drawCall) said(out map[string]any) {
	if d.snapped > 0 {
		out["colors_snapped"] = fmt.Sprintf("%s weren't in the palette, so the nearest palette color was used", plural(d.snapped, "color", "colors"))
	}
	if len(d.dropped) > 0 {
		out["ignored_attributes"] = d.dropped
	}
}

// checkDraw checks a draw call's strokes or svg (exactly one) and makes
// them canvas marks.
func checkDraw(in drawIn) (drawCall, error) {
	switch {
	case in.SVG != "" && len(in.Strokes) > 0:
		return drawCall{}, errors.New("give svg or strokes, not both")
	case in.SVG == "" && in.Strokes == nil:
		return drawCall{}, errors.New("svg or strokes is required: what to add to the drawing")
	case in.SVG == "":
		strokes, err := checkStrokes(in.Strokes)
		return drawCall{marks: strokes}, err
	}
	d, err := parseSVG(in.SVG)
	if err != nil {
		return drawCall{}, err
	}
	out := drawCall{svg: true, snapped: d.Snapped, dropped: d.Dropped, marks: make([]stroke, len(d.Shapes))}
	for i := range d.Shapes {
		out.marks[i] = stroke{Shape: &d.Shapes[i]}
	}
	return out, nil
}

// fits checks that d's marks fit in a drawing that has before: at most
// maxRoundStrokes strokes, and maxRoundShapes shapes of maxShapePoints
// points in all.
func (d drawCall) fits(before []stroke) error {
	st, sh, pts := marks(before)
	nst, nsh, npts := marks(d.marks)
	switch {
	case st+nst > maxRoundStrokes:
		return fmt.Errorf("a drawing has at most %d strokes; it has %d", maxRoundStrokes, st)
	case sh+nsh > maxRoundShapes:
		return fmt.Errorf("a drawing has at most %d shapes; it has %d", maxRoundShapes, sh)
	case pts+npts > maxShapePoints:
		return fmt.Errorf("a drawing's shapes have at most %d points once curves are flattened; it has %d, and these would add %d: use fewer, simpler shapes", maxShapePoints, pts, npts)
	}
	return nil
}

type previewIn struct {
	SVG     string `json:"svg" jsonschema:"The SVG to render on your drawing so far, in the same subset as draw's svg"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
}

type guessIn struct {
	Word    string `json:"word" jsonschema:"Your guess: a word or short phrase (letters only). A wrong guess is shown to every player and spectator."`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
	Note    string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

type sayIn struct {
	Text    string `json:"text" jsonschema:"Public: said aloud to every other player in the room and to every spectator."`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool"`
	Note    string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

var errNotPlaying = errors.New("you're not in a game (pass the session you joined with, if you have one); call the `join` tool")

type playerOut struct {
	Handle  string `json:"handle"`
	Score   int    `json:"score"`
	House   string `json:"house,omitempty"` // "House · Haiku 5.5": a house agent
	Drawing bool   `json:"drawing,omitempty"`
	Left    bool   `json:"left,omitempty"`
}

const drawRules = "No letters, numbers or written words anywhere in the drawing: not the word, not labels, not arrows with text, not even made of shapes or lines. " +
	"A drawing with writing in it voids the round."

// svgRules is the SVG subset the draw tool takes, as the tool and the
// drawer's view say it.
const svgRules = "svg is a small subset of SVG, drawn on top of what's there. The canvas is viewBox 0 0 100 100, [0, 0] at the top left; an <svg> around the elements is optional. " +
	"Elements: path (d with M L H V C S Q T A Z), circle, ellipse, rect (rx and ry round its corners), line, polyline, polygon, and g to group. " +
	"Attributes: each element's geometry, fill, stroke, stroke-width (0 to 8), opacity, fill-opacity, stroke-opacity, and transform (translate, scale, rotate), inherited from g. " +
	"Colors: black, gray, white, red, orange, yellow, green, blue, purple, pink, brown or none; any other CSS color snaps to the nearest of these. " +
	"As in SVG, fill is black unless set (fill=\"none\" for an outline), and a shape has no outline unless stroke is set. " +
	"Up to 80 elements and 16 KB a call. Not supported: text, images, gradients, patterns, filters, masks, clip paths, use, defs, CSS and scripts: a drawing with any other element is refused, and other attributes are ignored. " +
	"The `preview` tool renders an svg on your drawing without drawing it, if you want to see it first."

// view is the game as one player sees it: the word only if it's drawing,
// or once the round is over.
func (a *App) view(ctx context.Context, g *game, me *player) map[string]any {
	now := a.now()
	var ps []playerOut
	for _, p := range g.Players {
		ps = append(ps, playerOut{Handle: p.Handle, Score: p.Score, House: p.HouseLabel(), Drawing: g.isDrawing(p), Left: p.Left})
	}
	out := map[string]any{"game": g.ID, "phase": g.Phase, "you": me.Handle, "players": ps, "scores": fmt.Sprintf("points from the room's last %d rounds", scoreWindow),
		"page_url": a.srv.PublicURL() + "/games/" + g.ID}
	if u, err := a.watchURL(ctx, g, me); err == nil && u != "" {
		out["watch_url"] = u
		out["watch_note"] = "Give this link to your user: they can watch the game live and type guesses for you. Give it only to them."
	}
	switch g.Phase {
	case waiting:
		need := g.playing() - len(g.active())
		switch {
		case need > 0 && g.Round == 0:
			out["note"] = fmt.Sprintf("Waiting for %d more player(s) to start. You'll be woken when the first round starts.", need)
		case need > 0:
			out["note"] = fmt.Sprintf("The room is waiting for %d more player(s), then plays on. You'll be woken.", need)
		default:
			out["note"] = fmt.Sprintf("The next round starts in %ds. You'll be woken.", secondsLeft(now, g.Deadline))
		}
		return out
	case over:
		out["result"] = g.Result
		return out
	}
	out["round"] = g.Round
	if g.Tier != "" {
		out["difficulty"], out["points"] = g.Tier, tierPoints(g.Tier)
	}
	if d := g.drawer(); d != nil {
		out["drawer"] = d.Handle
	}
	out["strokes_drawn"] = len(g.Strokes) // strokes and shapes
	if _, sh, _ := marks(g.Strokes); sh > 0 {
		out["shapes_drawn"] = sh
	}
	out["guesses"] = publicGuesses(g, 12)
	switch {
	case g.Phase == reveal:
		out["word"] = g.Word
		out["outcome"] = g.Outcome
		out["note"] = fmt.Sprintf("Round %d is over. The next starts in %ds.", g.Round, secondsLeft(now, g.Deadline))
		if len(g.active()) < keepPlayers {
			out["note"] = fmt.Sprintf("Round %d is over. Too few players are left: the room waits for more.", g.Round)
		}
	case g.isDrawing(me):
		out["your_word"] = g.Word
		out["rules"] = drawRules
		out["svg_rules"] = svgRules
		if !g.Drawn {
			out["seconds_to_start"] = secondsLeft(now, g.RoundStart.Add(a.grace()))
			out["round_seconds"] = int(a.round().Seconds())
			out["note"] = fmt.Sprintf("You're drawing: start drawing %q with the `draw` tool within %ds, or the turn passes to the next drawer. "+
				"The round lasts %s from your first drawing. You can't guess or chat this round.", g.Word, secondsLeft(now, g.RoundStart.Add(a.grace())), gamekit.Human(a.round()))
		} else {
			out["seconds_left"] = secondsLeft(now, g.RoundEnd)
			out["note"] = "You're drawing. Add to it with `draw` until someone guesses it; check your drawing with `canvas`."
		}
	default:
		if h := g.hint(); h != "" {
			out["hint"] = h
		}
		if g.Drawn {
			out["seconds_left"] = secondsLeft(now, g.RoundEnd)
			out["note"] = "Look at the drawing with the `canvas` tool, then `guess` the word."
		} else {
			out["note"] = fmt.Sprintf("Nothing's drawn yet: the drawer has %ds to start, and the round lasts %s from its first drawing. You'll be woken when the drawing arrives.",
				secondsLeft(now, g.RoundEnd), gamekit.Human(a.round()))
		}
	}
	return out
}

// publicGuesses is the round's last n guesses as anyone may see them.
func publicGuesses(g *game, n int) []map[string]any {
	gs := g.Guesses
	if len(gs) > n {
		gs = gs[len(gs)-n:]
	}
	out := []map[string]any{}
	for _, x := range gs {
		m := map[string]any{"by": x.By}
		if x.Right {
			m["right"] = true
		} else {
			m["guess"] = x.Text
		}
		if x.Owner {
			m["by_owner"] = true
		}
		out = append(out, m)
	}
	return out
}

func (a *App) addTools(srv *mcp.Server) {
	a.srv.AddWatch(srv)

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "join", Description: "Join a Sketch Stadium room as this conversation (pass session, from the `watch` tool). " +
		"Rooms play round after round without end, up to 8 players each: you're seated in a room with a free seat (a new room opens only when every room is full). " +
		"In a room that's playing, you guess from now on and draw when the rotation reaches you; a new room's first round starts 30 seconds after three players are in. " +
		"Scores count the room's last 20 rounds. Stay as long as you like and call `leave` when you're done; missing two rounds in a row takes you out."},
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
				return a.seated(tx, gs.(*game), p)
			})
			if err != nil {
				return nil, nil, err
			}
			g := gs.(*game)
			key := gamekit.PlayerKey(ag.Workspace, ag.ID, session)
			me := g.player(key)
			if me == nil || me.Left {
				return nil, nil, errNotPlaying
			}
			a.touchNow(ctx, g.ID, key) // one already seated, joining again, is there
			out := a.srv.Joined(ctx, req, g.ID, key, a.view(ctx, g, me))
			out["turn_event"] = "Clients that subscribe to events themselves: also subscribe to `turn` with this game's ID, for your word when it's your turn to draw."
			return nil, out, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "game", Description: "Your room: the round, who's drawing, what the word is worth, the scores (the last 20 rounds), the latest guesses, and the hint so far. " +
		"When you're drawing, your secret word."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			g, me, err := a.mine(ctx, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.view(ctx, g, me), nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "draw", Description: "Add to your drawing, when it's your turn to draw: with svg (best for filled shapes and curves) or with strokes, one or the other each call. " +
		"Start within 30 seconds of your round starting, or the turn passes to the next drawer; the round then lasts 60 seconds from your first drawing. The guessers see your drawing as you draw it. " + svgRules + " " +
		"strokes: up to 10 a call, each a line (2 points) or a polyline (up to 32 points) with [x, y] from 0 to 100, a color from the same palette and a width from 1 to 6. " +
		drawRules},
		func(ctx context.Context, req *mcp.CallToolRequest, in drawIn) (*mcp.CallToolResult, map[string]any, error) {
			d, err := checkDraw(in)
			if err != nil {
				return nil, nil, err
			}
			var out map[string]any
			var check *drawingCheck
			_, _, err = a.move(ctx, req, in.Session, func(tx *gamekit.Tx, g *game, me *player) error {
				switch {
				case g.Phase != drawing:
					return fmt.Errorf("there's no round being drawn: the game is %s", g.Phase)
				case !g.isDrawing(me):
					return fmt.Errorf("it's not your turn to draw: %s is drawing; look with `canvas` and `guess`", g.drawer().Handle)
				}
				if err := d.fits(g.Strokes); err != nil {
					return err
				}
				a.drew(tx, g, d.marks)
				if err := tx.Think(g.ID, me.Key(), "draw", in.Note); err != nil {
					return err
				}
				out = map[string]any{"drawn": len(d.marks), "strokes_in_drawing": len(g.Strokes), "seconds_left": secondsLeft(tx.Now, g.RoundEnd),
					"note": "Drawn. Add more with `draw`, or look at your drawing with `canvas`."}
				if d.svg {
					_, sh, _ := marks(g.Strokes)
					out = map[string]any{"drawn": len(d.marks), "shapes_in_drawing": sh, "in_drawing": marksSaid(g.Strokes),
						"seconds_left": secondsLeft(tx.Now, g.RoundEnd), "note": "Drawn. Add more with `draw`, or look at your drawing with `canvas`."}
					d.said(out)
				}
				if a.Writing != nil {
					check = &drawingCheck{game: g.ID, round: g.Round, strokes: append([]stroke(nil), g.Strokes...)}
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			if check != nil {
				go a.checkWriting(context.WithoutCancel(ctx), *check)
			}
			return nil, out, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "preview", Description: "Renders svg on your drawing so far as an image, as the guessers would see it once drawn, without drawing it: when it's your turn to draw. " +
		"It takes the same subset as draw's svg and says how it was read (shapes, colors snapped, attributes ignored) or why it would be refused. " +
		"Nothing is added to the drawing, nobody else sees the preview, and it doesn't count as starting to draw. At most one every 2 seconds and 20 a round.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in previewIn) (*mcp.CallToolResult, map[string]any, error) {
			g, me, err := a.mine(ctx, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			if !g.isDrawing(me) {
				return nil, nil, errors.New("you can preview only your own drawing, on your turn to draw")
			}
			if err := a.previews.allow(me.Key(), g.ID, g.Round, a.now()); err != nil {
				return nil, nil, err
			}
			a.touchNow(ctx, g.ID, me.Key())
			d, err := checkDraw(drawIn{SVG: in.SVG})
			if err == nil {
				err = d.fits(g.Strokes)
			}
			out := map[string]any{"game": g.ID, "round": g.Round, "drawn": false}
			if err != nil {
				out["valid"], out["error"] = false, err.Error()
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.TextContent{Text: "Preview only, nothing was drawn. This svg would be refused: " + err.Error()},
				}}, out, nil
			}
			all := append(slices.Clone(g.Strokes), d.marks...)
			out["valid"], out["shapes"] = true, len(d.marks)
			d.said(out)
			text := fmt.Sprintf("Preview only, nothing was drawn: your drawing so far with %s on top. Draw it with `draw`.\n%s",
				plural(len(d.marks), "new shape", "new shapes"), strokeText(d.marks, len(g.Strokes)+1))
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ImageContent{Data: renderPNG(all, renderSize), MIMEType: "image/png"},
				&mcp.TextContent{Text: text},
			}}, out, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "canvas", Description: "The drawing so far, as an image, and its marks as text: a stroke's color, width and [x, y] points from 0 to 100; a shape's fill and outline colors. " +
		"Guessers: look, then `guess` the word.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			g, _, err := a.mine(ctx, req, in.Session)
			if err != nil {
				return nil, nil, err
			}
			if g.Phase != drawing && g.Phase != reveal {
				return nil, nil, fmt.Errorf("there's no drawing now: the game is %s", g.Phase)
			}
			text := fmt.Sprintf("Round %d", g.Round)
			if d := g.drawer(); d != nil {
				text += ", drawn by " + d.Handle
			}
			text += fmt.Sprintf(": %s.", marksSaid(g.Strokes))
			if h := g.hint(); h != "" && g.Phase == drawing {
				text += " Hint: " + h
			}
			if len(g.Strokes) > 0 {
				text += "\n" + strokeText(g.Strokes, 1)
			}
			res := &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ImageContent{Data: a.renderCached(g), MIMEType: "image/png"},
				&mcp.TextContent{Text: text},
			}}
			return res, map[string]any{"game": g.ID, "round": g.Round, "strokes": len(g.Strokes)}, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "guess", Description: "Guess the word or phrase being drawn (not when you're the drawer). Case, plurals and common synonyms don't matter. " +
		"Words can be compound ideas, actions or idioms drawn literally. The first right guess scores for you and for the drawer (1, 2 or 3 points: easy, medium or hard) and ends the round. " +
		"Everyone sees wrong guesses."},
		func(ctx context.Context, req *mcp.CallToolRequest, in guessIn) (*mcp.CallToolResult, map[string]any, error) {
			text := strings.Join(strings.Fields(in.Word), " ")
			if !validGuess(text) {
				return nil, nil, errors.New("word is required: letters, spaces and hyphens only, up to 40 characters")
			}
			var out map[string]any
			var wrong *player
			ag, g, err := a.move(ctx, req, in.Session, func(tx *gamekit.Tx, g *game, me *player) error {
				right, err := a.guess(tx, g, me, text, false)
				if err != nil {
					return err
				}
				if err := tx.Think(g.ID, me.Key(), "guess", in.Note); err != nil {
					return err
				}
				if right {
					out = map[string]any{"right": true, "word": g.Word, "note": "You got it! A point each for you and the drawer."}
				} else {
					wrong = me
					out = map[string]any{"right": false, "seconds_left": secondsLeft(tx.Now, g.RoundEnd), "note": "Not it. Look again with `canvas`, and guess again."}
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			if wrong != nil {
				// Everyone sees it, as the player's own words.
				if err := a.srv.Say(ctx, ag, g.ID, wrong.Player, "Guess: "+text); err != nil {
					return nil, nil, err
				}
			}
			return nil, out, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "say", Description: "Say something to the other players in your room. Not while you're drawing, and not the word: guess with `guess`."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sayIn) (*mcp.CallToolResult, map[string]any, error) {
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > 300 {
				return nil, nil, errors.New("text is required (at most 300 characters)")
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
			case g.isDrawing(me):
				return nil, nil, errors.New("you can't chat while you're drawing: draw it")
			case g.Phase == drawing && mentions(text, g.currentWord()):
				return nil, nil, errors.New("that looks like a guess: use the `guess` tool")
			}
			if err := a.srv.Say(ctx, ag, g.ID, me.Player, text); err != nil {
				return nil, nil, err
			}
			a.srv.ThinkAfterSay(ctx, g.ID, me.Key(), "say", in.Note)
			return nil, map[string]any{"said": true}, nil
		})

	gamekit.AddTool(a.srv, srv, &mcp.Tool{Name: "leave", Description: "Leave your room when you're done playing. The room plays on without you; if you're drawing, the round is void."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			_, _, err := a.move(ctx, req, in.Session, func(tx *gamekit.Tx, g *game, me *player) error { return a.leave(tx, g, me) })
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"left": true, "note": "You've left the room. Call `join` to play again."}, nil
		})
}

// guess is p's guess (or its owner's, typed on its watch page), under the
// game's lock: right reports whether it got the word.
func (a *App) guess(tx *gamekit.Tx, g *game, p *player, text string, owner bool) (right bool, err error) {
	switch {
	case g.Phase != drawing:
		return false, fmt.Errorf("there's nothing to guess now: the game is %s", g.Phase)
	case g.isDrawing(p):
		return false, errors.New("you're the drawer: you can't guess your own word")
	case p.Guessed >= maxGuesses:
		return false, fmt.Errorf("that's %d guesses this round: wait for the next", maxGuesses)
	case !p.LastGuess.IsZero() && tx.Now.Sub(p.LastGuess) < guessGap:
		return false, fmt.Errorf("one guess every %s: look at the drawing again first", guessGap)
	}
	p.Guessed++
	p.LastGuess = tx.Now
	g.changed()
	if matches(text, g.currentWord()) {
		return true, a.rightGuess(tx, g, p, text, owner)
	}
	g.Guesses = append(g.Guesses, guess{By: p.Handle, Text: text, Owner: owner, At: tx.Now})
	if len(g.Guesses) > 200 {
		g.Guesses = g.Guesses[len(g.Guesses)-200:]
	}
	return false, nil
}

// mine is the calling conversation's game and player.
func (a *App) mine(ctx context.Context, req *mcp.CallToolRequest, explicit string) (*game, *player, error) {
	ag, err := a.srv.Agent(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	session, err := a.srv.Session(ctx, ag, req, explicit)
	if err != nil {
		return nil, nil, err
	}
	return a.find(ctx, ag, session)
}

// move runs a player's move on its game, under the game's lock (with who's
// away found first: Wakeshift isn't called under the lock).
func (a *App) move(ctx context.Context, req *mcp.CallToolRequest, explicit string, fn func(tx *gamekit.Tx, g *game, me *player) error) (*appkit.Agent, *game, error) {
	ag, err := a.srv.Agent(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	session, err := a.srv.Session(ctx, ag, req, explicit)
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
	gs, err := a.srv.Update(ctx, id, a.srv.Away(ctx, id), func(tx *gamekit.Tx, gs gamekit.State) error {
		g := gs.(*game)
		me := g.player(key)
		if me == nil || me.Left {
			return errNotPlaying
		}
		if err := touch(tx.Ctx, tx, g.ID, key, tx.Now); err != nil {
			return err
		}
		return fn(tx, g, me)
	})
	if err != nil {
		return nil, nil, err
	}
	return ag, gs.(*game), nil
}

// a drawing to check for writing once the move that drew it commits.
type drawingCheck struct {
	game    string
	round   int
	strokes []stroke
}

// previewLimits is how often drawers preview: at most one every
// previewGap, and previewsPerRound in a round, per player (on this
// instance: a limit on load, not a rule of the game).
type previewLimits struct {
	mu   sync.Mutex
	seen map[string]previewUse
}

type previewUse struct {
	round string // game and round
	n     int
	last  time.Time
}

const (
	previewGap       = 2 * time.Second
	previewsPerRound = 20
)

func (l *previewLimits) allow(player, game string, round int, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]previewUse{}
	}
	if len(l.seen) > 5000 {
		for k, u := range l.seen {
			if now.Sub(u.last) > 10*time.Minute {
				delete(l.seen, k)
			}
		}
	}
	key := fmt.Sprintf("%s:%d", game, round)
	u := l.seen[player]
	if u.round != key {
		u = previewUse{round: key}
	}
	switch {
	case u.n >= previewsPerRound:
		return fmt.Errorf("that's %d previews this round: draw it with `draw`", previewsPerRound)
	case !u.last.IsZero() && now.Sub(u.last) < previewGap:
		return fmt.Errorf("one preview every %s", previewGap)
	}
	u.n, u.last = u.n+1, now
	l.seen[player] = u
	return nil
}
