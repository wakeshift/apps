// Command sketchtool is Sketch Stadium's rules, renderer and writing check
// for the house agents' model evaluations: it reads one JSON
// request a line on stdin and writes one JSON answer a line on stdout,
// with the request's id. Requests run at once; answers come as they finish.
//
//	{"id": 1, "op": "draw", "args": {"strokes": [...]} or {"svg": "..."}, "before": [...]}
//	  → {"id": 1, "strokes": [...], "png": "<base64>"} or {"id": 1, "error": "..."}
//	    the call checked as the draw tool checks it, and the canvas after it
//	    (before's strokes and the call's) as the canvas tool shows it
//	{"id": 2, "op": "render", "strokes": [...]} → {"id": 2, "png": "<base64>"}
//	{"id": 3, "op": "match", "guess": "Bananas", "word": "banana"} → {"id": 3, "valid": true, "right": true}
//	{"id": 4, "op": "words"} → {"id": 4, "words": [{"word", "tier", "synonyms"}...]}
//	{"id": 5, "op": "writing", "png": "<base64>"} → {"id": 5, "writing": false}
//	    the game's writing check (cheat.go), with SKETCH_WRITING_KEY and
//	    SKETCH_WRITING_MODEL (default claude-haiku-5-5) from the environment
//
// With the argument png, it reads one drawing (a JSON array of marks) on
// stdin and writes it as a PNG on stdout: an archived round's drawing
// (sketch_rounds.drawing, apps/sketch/archive.go), for instance
//
//	psql "$DB" -Atc 'SELECT drawing FROM sketch_rounds WHERE id = 42' | sketchtool png > round.png
//
// Strokes are a drawing's marks as the game stores them: strokes {"p": [[x, y], ...],
// "c": color, "w": width}, and an svg call's shapes {"sh": {...}} (sketch.CheckDraw).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wakeshift/apps/sketch"
)

type request struct {
	ID      int             `json:"id"`
	Op      string          `json:"op"`
	Args    json.RawMessage `json:"args,omitempty"`
	Before  json.RawMessage `json:"before,omitempty"`
	Strokes json.RawMessage `json:"strokes,omitempty"`
	Guess   string          `json:"guess,omitempty"`
	Word    string          `json:"word,omitempty"`
	PNG     string          `json:"png,omitempty"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "png" {
		if err := pngOf(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "sketchtool png:", err)
			os.Exit(1)
		}
		return
	}
	model := os.Getenv("SKETCH_WRITING_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	writing := &sketch.AnthropicWriting{Model: model, Key: os.Getenv("SKETCH_WRITING_KEY")}
	out := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	var wg sync.WaitGroup
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for in.Scan() {
		var r request
		if err := json.Unmarshal(in.Bytes(), &r); err != nil {
			mu.Lock()
			out.Encode(map[string]any{"error": "request: " + err.Error()})
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := answer(r, writing)
			a["id"] = r.ID
			mu.Lock()
			out.Encode(a)
			mu.Unlock()
		}()
	}
	wg.Wait()
}

func answer(r request, writing *sketch.AnthropicWriting) map[string]any {
	fail := func(err error) map[string]any { return map[string]any{"error": err.Error()} }
	switch r.Op {
	case "draw":
		strokes, err := sketch.CheckDraw(r.Args, r.Before)
		if err != nil {
			return fail(err)
		}
		all, err := joinStrokes(r.Before, strokes)
		if err != nil {
			return fail(err)
		}
		png, err := sketch.RenderPNG(all)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"strokes": json.RawMessage(strokes), "png": base64.StdEncoding.EncodeToString(png)}
	case "render":
		png, err := sketch.RenderPNG(r.Strokes)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"png": base64.StdEncoding.EncodeToString(png)}
	case "match":
		valid, right, err := sketch.MatchGuess(r.Guess, r.Word)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"valid": valid, "right": right}
	case "words":
		return map[string]any{"words": sketch.Words()}
	case "writing":
		if writing.Key == "" {
			return fail(errors.New("SKETCH_WRITING_KEY isn't set"))
		}
		png, err := base64.StdEncoding.DecodeString(r.PNG)
		if err != nil {
			return fail(err)
		}
		// As the game runs it: 15 seconds, and an error counts as no writing there.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		w, err := writing.HasWriting(ctx, png)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"writing": w}
	}
	return fail(errors.New("unknown op " + r.Op))
}

// pngOf renders the drawing read from in as a PNG written to out.
func pngOf(in io.Reader, out io.Writer) error {
	b, err := io.ReadAll(io.LimitReader(in, 64<<20))
	if err != nil {
		return err
	}
	png, err := sketch.RenderPNG(bytes.TrimSpace(b))
	if err != nil {
		return err
	}
	_, err = out.Write(png)
	return err
}

// joinStrokes is two JSON arrays of strokes as one.
func joinStrokes(a, b json.RawMessage) (json.RawMessage, error) {
	var x, y []json.RawMessage
	if len(a) > 0 {
		if err := json.Unmarshal(a, &x); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return nil, err
	}
	return json.Marshal(append(x, y...))
}
