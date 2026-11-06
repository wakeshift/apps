package wakeshift

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Wakeshift trims a text over 1000 bytes rather than refusing it: Emit
// sends the text as Text renders it, however long, reports TextTrimmed,
// and logs one warning per event.
func TestEmitTextTrimmed(t *testing.T) {
	var logMu sync.Mutex
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warnings := func() []string {
		logMu.Lock()
		defer logMu.Unlock()
		var out []string
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["level"] == "WARN" {
				out = append(out, m["msg"].(string))
			}
		}
		return out
	}

	var mu sync.Mutex
	trimmed := true
	var texts []string
	app, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/events/emit") {
			io.WriteString(w, `{}`)
			return
		}
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		texts = append(texts, body.Text)
		if trimmed {
			io.WriteString(w, `{"matched":1,"duplicate":false,"text_trimmed":true}`)
		} else {
			io.WriteString(w, `{"matched":1,"duplicate":false}`)
		}
	}))
	long := strings.Repeat("x", 5000)
	text := func(a internalArgs, _ internalArgs) string { return a.Table }
	turn, err := DefineEvent(app, "t.turn", EventDef[internalArgs, internalArgs]{Description: "E.", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	news, err := DefineEvent(app, "t.news", EventDef[internalArgs, internalArgs]{Description: "E.", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		out, err := turn.Emit(ctx, internalArgs{Table: "oak"}, internalArgs{Table: long})
		if err != nil || !out.TextTrimmed {
			t.Fatalf("emit: %+v %v", out, err)
		}
	}
	if texts[0] != long {
		t.Fatalf("sent %d bytes of text, want the %d Text rendered", len(texts[0]), len(long))
	}
	if w := warnings(); len(w) != 1 || !strings.Contains(w[0], "t.turn's text was over 1000 bytes") {
		t.Fatalf("warnings: %q", w)
	}
	if out, err := news.Emit(ctx, internalArgs{Table: "oak"}, internalArgs{Table: long}); err != nil || !out.TextTrimmed {
		t.Fatalf("news: %+v %v", out, err)
	}
	if w := warnings(); len(w) != 2 || !strings.Contains(w[1], "t.news") {
		t.Fatalf("warnings: %q", w)
	}
	mu.Lock()
	trimmed = false
	mu.Unlock()
	if out, err := turn.Emit(ctx, internalArgs{Table: "oak"}, internalArgs{Table: "hi"}); err != nil || out.TextTrimmed {
		t.Fatalf("short: %+v %v", out, err)
	}
	if w := warnings(); len(w) != 2 {
		t.Fatalf("warnings: %q", w)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// An event without Text: the line the agent reads, as every SDK writes it
// (default-text.json).
func TestDefaultTextGolden(t *testing.T) {
	var f struct {
		Cases []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Args        json.RawMessage `json:"args"`
			Want        string          `json:"want"`
		} `json:"cases"`
	}
	readSharedFixture(t, "default-text.json", &f)
	if len(f.Cases) < 10 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		if got := defaultText(c.Description, c.Args); got != c.Want {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Want)
		}
	}
}
