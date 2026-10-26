package sketch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

type alwaysWriting bool

func (w alwaysWriting) HasWriting(context.Context, []byte) (bool, error) { return bool(w), nil }

// A drawing the check finds writing in voids the round: nobody scores, and
// the word is revealed. One it doesn't leaves the round as it was.
func TestWritingVoidsTheRound(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	f.set(f.clock().Add(time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 9, 9)}); return nil })
	check := drawingCheck{game: g.ID, round: g.Round, strokes: g.Strokes}

	f.a.Writing = alwaysWriting(false)
	f.a.checkWriting(bg, check)
	if g := f.load(); g.Phase != drawing {
		t.Fatalf("a clean drawing: %s", g.Phase)
	}
	f.a.Writing = alwaysWriting(true)
	f.a.checkWriting(bg, check)
	g = f.load()
	if g.Phase != reveal || g.Outcome != "writing" || !strings.Contains(f.lastSaid(), "letters or numbers") || !strings.Contains(f.lastSaid(), g.Word) {
		t.Fatalf("after writing: %s %s %q", g.Phase, g.Outcome, f.lastSaid())
	}
	for _, p := range g.Players {
		if p.Score != 0 {
			t.Fatalf("a void round scored: %+v", p)
		}
	}
	// A check that comes back after the round has moved on changes nothing.
	f.a.checkWriting(bg, check)
	if n := strings.Count(f.said(), "letters or numbers"); n != 1 {
		t.Fatalf("voided %d times", n)
	}
}

// The Anthropic check sends the drawing as an image, with the key only in
// its header, and reads YES as writing.
func TestAnthropicWriting(t *testing.T) {
	var got struct {
		key   string
		image string
	}
	answer := "YES"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Thinking struct{ Type string }
			Messages []struct {
				Content []struct {
					Type   string
					Source struct {
						MediaType string `json:"media_type"`
					}
				}
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		got.key = r.Header.Get("x-api-key")
		got.image = body.Messages[0].Content[0].Source.MediaType
		if body.Thinking.Type != "disabled" {
			// As Haiku 5.5 answers with its thinking on: five tokens of thinking, no words.
			json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": ""}}, "stop_reason": "max_tokens"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}})
	}))
	t.Cleanup(api.Close)
	w := &AnthropicWriting{Model: "claude-haiku-4-5-20251001", Key: "sk-test", Base: api.URL}
	png := renderPNG([]stroke{line(1, 1, 50, 50)}, 64)
	if yes, err := w.HasWriting(bg, png); err != nil || !yes || got.key != "sk-test" || got.image != "image/png" {
		t.Fatalf("%v %v %+v", yes, err, got)
	}
	answer = "No."
	if yes, err := w.HasWriting(bg, png); err != nil || yes {
		t.Fatalf("NO read as %v %v", yes, err)
	}
	// No key: no check.
	if yes, err := (&AnthropicWriting{Model: "m", Base: api.URL}).HasWriting(bg, png); yes || err != nil {
		t.Fatalf("keyless: %v %v", yes, err)
	}
}
