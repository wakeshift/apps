package sketch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/wakeshift/apps/gamekit"
)

// The game's rules and renderer for tools outside it (cmd/sketchtool, for
// house's model bake-off): a draw call's arguments checked as the draw tool
// checks them, a drawing rendered as the canvas tool renders it, and a
// guess matched as the guess tool matches it. Drawings go in and out as the
// game stores them: strokes {"p": [[x, y], …], "c": color, "w": width} and
// shapes {"sh": {"d": [{"p": [[x, y], …], "z": closed}], "f": fill, "c":
// outline, "w": width}}.

// Word is a word drawers get, its tier (easy, medium or hard) and the
// other words a guess may use for it.
type Word struct {
	Word     string   `json:"word"`
	Tier     string   `json:"tier"`
	Synonyms []string `json:"synonyms,omitempty"`
}

// Words is the game's word list, in order.
func Words() []Word {
	out := make([]Word, len(words))
	for i, w := range words {
		out[i] = Word{w.Word, w.Tier, append([]string(nil), w.Synonyms...)}
	}
	return out
}

// CheckDraw checks a draw call's arguments as the draw tool does: its
// schema (no unknown arguments, a whole-number width), then its strokes or
// svg (checkDraw), then the round's caps with before (marks already drawn,
// as CheckDraw returns them). It returns the call's marks as the game
// stores them: strokes, or shapes ({"sh": …}) for svg.
func CheckDraw(args []byte, before []byte) ([]byte, error) {
	var in drawIn
	args = gamekit.NoteArgs(args, true) // the note's old name, reasoning, as the tool takes it
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("arguments: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("arguments: one JSON object, nothing after it")
	}
	d, err := checkDraw(in)
	if err != nil {
		return nil, err
	}
	prior, err := decodeStrokes(before)
	if err != nil {
		return nil, err
	}
	if err := d.fits(prior); err != nil {
		return nil, err
	}
	return json.Marshal(d.marks)
}

// RenderPNG is the canvas as the canvas tool shows it (512 pixels a side),
// for marks as the game stores them.
func RenderPNG(strokes []byte) ([]byte, error) {
	s, err := decodeStrokes(strokes)
	if err != nil {
		return nil, err
	}
	known := func(c string) bool { _, ok := palette[c]; return ok }
	for i, st := range s {
		if sh := st.Shape; sh != nil {
			if (sh.Fill != "" && !known(sh.Fill)) || (sh.Color != "" && !known(sh.Color)) {
				return nil, fmt.Errorf("shape %d: a color not in the palette", i+1)
			}
			continue
		}
		if !known(st.Color) {
			return nil, fmt.Errorf("stroke %d: no color %q", i+1, st.Color)
		}
	}
	if _, sh, pts := marks(s); len(s) > maxRoundStrokes+maxRoundShapes || sh > maxRoundShapes || pts > maxShapePoints {
		return nil, errors.New("too many marks or points")
	}
	return renderPNG(s, renderSize), nil
}

// MatchGuess is a guess as the guess tool takes it: whether it's a valid
// guess at all, and whether it names name (a word in the list) or one of
// its synonyms, whatever its case, article or plural.
func MatchGuess(guess, name string) (valid, right bool, err error) {
	i := slices.IndexFunc(words, func(w word) bool { return w.Word == name })
	if i < 0 {
		return false, false, errors.New("no such word")
	}
	text := strings.Join(strings.Fields(guess), " ") // as the guess tool tidies it
	if !validGuess(text) {
		return false, false, nil
	}
	return true, matches(text, words[i]), nil
}

func decodeStrokes(b []byte) ([]stroke, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var s []stroke
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("strokes: %v", err)
	}
	return s, nil
}
