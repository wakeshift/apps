package gamekit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

type noIn struct{}

// Under load a tool says the game is busy, rather than failing: when every
// slot is taken, and when the database or Wakeshift is overloaded. Other
// database errors don't reach the agent; the game's own errors do.
func TestBusy(t *testing.T) {
	ctx := context.Background()
	s := &Server{Title: "Nightfall", Name: "nightfall", MaxCalls: 1}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t"}, nil)
	release := make(chan struct{})
	started := make(chan struct{})
	AddTool(s, srv, &mcp.Tool{Name: "slow"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, map[string]any, error) {
		close(started)
		<-release
		return nil, map[string]any{"ok": true}, nil
	})
	fail := map[string]error{
		"full":    &pgconn.PgError{Code: "53300", Message: "too many connections"},
		"timeout": fmt.Errorf("acquiring: %w", context.DeadlineExceeded),
		"core":    &appkit.APIError{Status: 503, Code: "unavailable"},
		"limited": &appkit.APIError{Status: 429, Code: "rate_limited"},
		"sql":     &pgconn.PgError{Code: "42P01", Message: `relation "games" does not exist`},
		"panic":   nil,
		"game":    errors.New("it's not night: the game is day"),
	}
	for name, err := range fail {
		AddTool(s, srv, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, noIn) (*mcp.CallToolResult, map[string]any, error) {
			if name == "panic" {
				panic("a bug")
			}
			return nil, nil, err
		})
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(tool string) string {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			return "ok"
		}
		return res.Content[0].(*mcp.TextContent).Text
	}

	busy := "Nightfall is busy right now. Wait a few seconds and call the tool again"
	for _, name := range []string{"full", "timeout", "core", "limited"} {
		if got := call(name); !strings.HasPrefix(got, busy) {
			t.Errorf("%s: %q", name, got)
		}
	}
	for _, name := range []string{"sql", "panic"} {
		if got := call(name); got != "something went wrong on our side; try again" {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := call("game"); got != "it's not night: the game is day" {
		t.Errorf("game: %q", got)
	}

	// The one slot is taken: the next call waits a moment, then is told.
	done := make(chan string)
	go func() { done <- call("slow") }()
	<-started
	start := time.Now()
	if got := call("game"); !strings.HasPrefix(got, busy) || time.Since(start) < slotWait {
		t.Errorf("with no free slot: %q after %s", got, time.Since(start))
	}
	close(release)
	if got := <-done; got != "ok" {
		t.Errorf("slow: %q", got)
	}
}
