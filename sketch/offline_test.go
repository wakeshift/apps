package sketch

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckDraw(t *testing.T) {
	ok, err := CheckDraw([]byte(`{"note": "a sun", "strokes": [{"points": [[10, 10.4], [90, 90]], "color": "Yellow", "width": 4}, {"points": [[50, 50]]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(ok), `[{"p":[[10,10],[90,90]],"c":"yellow","w":4},{"p":[[50,50]],"c":"black","w":2}]`; got != want {
		t.Fatalf("strokes = %s, want %s", got, want)
	}
	tooMany := make([]map[string]any, 11)
	for i := range tooMany {
		tooMany[i] = map[string]any{"points": [][]float64{{1, 1}}}
	}
	many, _ := json.Marshal(map[string]any{"strokes": tooMany})
	long := make([][]int, 33)
	for i := range long {
		long[i] = []int{i, i}
	}
	longJSON, _ := json.Marshal(map[string]any{"strokes": []any{map[string]any{"points": long}}})
	full := bytes.Repeat([]byte(`{"p":[[1,1]],"c":"black","w":2},`), 195)
	full = append(append([]byte("["), full[:len(full)-1]...), ']')
	for name, c := range map[string]struct {
		args, before string
		want         string
	}{
		"out of bounds":   {`{"strokes": [{"points": [[0, 0], [101, 50]]}]}`, "", "x and y are 0 to 100"},
		"negative":        {`{"strokes": [{"points": [[-1, 0]]}]}`, "", "x and y are 0 to 100"},
		"color":           {`{"strokes": [{"points": [[1, 1]], "color": "teal"}]}`, "", "no color"},
		"width":           {`{"strokes": [{"points": [[1, 1]], "width": 7}]}`, "", "width is 1 to 6"},
		"fractional":      {`{"strokes": [{"points": [[1, 1]], "width": 2.5}]}`, "", "arguments"},
		"unknown field":   {`{"strokes": [{"points": [[1, 1]], "fill": "red"}]}`, "", "unknown field"},
		"strokes as text": {`{"strokes": "[{\"points\": [[1, 1]]}]"}`, "", "arguments"},
		"no strokes":      {`{"strokes": []}`, "", "at least one"},
		"trailing":        {`{"strokes": [{"points": [[1, 1]]}]} {}`, "", "nothing after it"},
		"too many":        {string(many), "", "at most 10 strokes"},
		"long stroke":     {string(longJSON), "", "1 to 32 points"},
		"three numbers":   {`{"strokes": [{"points": [[1, 1, 1]]}]}`, "", "a point is [x, y]"},
		"round cap":       {`{"strokes": [{"points": [[1, 1]]}, {"points": [[1, 1]]}, {"points": [[1, 1]]}, {"points": [[1, 1]]}, {"points": [[1, 1]]}, {"points": [[1, 1]]}]}`, string(full), "at most 200 strokes"},
	} {
		_, err := CheckDraw([]byte(c.args), []byte(c.before))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// The bake-off's images are the canvas tool's, pixel for pixel.
func TestRenderPNGIsTheCanvas(t *testing.T) {
	strokes := []stroke{{Points: [][2]int{{10, 10}, {90, 50}, {20, 80}}, Color: "red", Width: 3}, {Points: [][2]int{{50, 50}}, Color: "blue", Width: 6}}
	b, _ := json.Marshal(strokes)
	got, err := RenderPNG(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, renderPNG(strokes, renderSize)) {
		t.Fatal("RenderPNG differs from the canvas tool's image")
	}
	if _, err := RenderPNG([]byte(`[{"p":[[1,1]],"c":"teal","w":2}]`)); err == nil {
		t.Fatal("rendered a color the game hasn't")
	}
}

func TestMatchGuess(t *testing.T) {
	for _, c := range []struct {
		guess, word  string
		valid, right bool
	}{
		{"The Bananas!", "banana", false, false}, // "!" isn't a guess the tool takes
		{"The  Bananas", "banana", true, true},
		{"kitten", "cat", true, true},
		{"dog", "cat", true, false},
		{"circus tent", "big top", true, true},
		{"42", "cat", false, false},
	} {
		valid, right, err := MatchGuess(c.guess, c.word)
		if err != nil || valid != c.valid || right != c.right {
			t.Errorf("MatchGuess(%q, %q) = %v, %v, %v; want %v, %v", c.guess, c.word, valid, right, err, c.valid, c.right)
		}
	}
	if _, _, err := MatchGuess("x", "no such word here"); err == nil {
		t.Error("matched a word not in the list")
	}
	if len(Words()) != len(words) {
		t.Error("Words isn't the list")
	}
}
