package wakeshift_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// The examples run on a disabled client (no key): offline and the same
// every time. With WAKESHIFT_KEY set, the same code is on.

type jobArgs struct {
	Job string `json:"job"`
}

type crawl struct {
	Status string `json:"status"`
	Pages  int    `json:"pages"`
}

type startIn struct {
	URL string `json:"url"`
}

type startOut struct {
	Job string `json:"job"`
}

// The quickstart: a crawler that wakes the conversation that started a
// crawl.
func Example() {
	wake, err := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		log.Fatal(err) // a malformed key
	}
	crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
		Description: "A crawl finished",
	})
	if err != nil {
		log.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
		func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, startOut, error) {
			job := "3f2a" // your tool minted the ID, so only this conversation can know it
			if _, err := crawlFinished.Subscribe(ctx, call, jobArgs{Job: job}, wakeshift.Once()); err != nil {
				return nil, startOut{}, err
			}
			return nil, startOut{Job: job}, nil
		}, wakeshift.Subscribes(crawlFinished))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	http.Handle("/example/mcp", attached.Handler(h))

	// When the crawl finishes:
	got, err := crawlFinished.Emit(context.Background(), jobArgs{Job: "3f2a"}, crawl{Status: "completed", Pages: 12})
	fmt.Println(got.Matched, err)
	// Output: 0 <nil>
}

func ExampleNew() {
	// New reads WAKESHIFT_KEY and does no I/O. Without a key the client is
	// disabled: the server runs as before, and nothing is sent.
	wake, err := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	fmt.Println(wake.Enabled(), err)
	// A malformed key is an error, which never quotes the key.
	_, err = wakeshift.New(wakeshift.WithKey("wst_not-a-key"))
	fmt.Println(wakeshift.IsCode(err, wakeshift.CodeConfig))
	// Output:
	// false <nil>
	// true
}

func ExampleDefineEvent() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	type channel struct {
		Channel string `json:"channel" jsonschema:"The channel"`
	}
	type message struct {
		From string `json:"from"`
		Text string `json:"text"`
	}
	// Authorize opens the event to subscribe_events and native clients,
	// gated: here, members of the channel.
	posted, err := wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[channel, message]{
		Description: "A new message in a channel",
		Text:        func(m message, c channel) string { return m.From + " in #" + c.Channel + ": " + m.Text },
		Authorize: func(ctx context.Context, s wakeshift.Subscriber, c channel) (bool, error) {
			return s.User != "" && c.Channel == "eng", nil // your own membership check
		},
	})
	fmt.Println(posted.Name(), err)
	// Output: chat.message <nil>
}

func ExampleEvent_Emit() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	turn, _ := wakeshift.DefineEvent(wake, "game.turn", wakeshift.EventDef[jobArgs, crawl]{Description: "Your move", Personal: true})
	// A personal event goes to the users your own records name.
	got, err := turn.Emit(context.Background(), jobArgs{Job: "t1"}, crawl{}, wakeshift.To("ada"), wakeshift.WithID("t1-move-3"))
	fmt.Println(got.Matched, err)
	// Without To, it's refused before anything is sent.
	_, err = turn.Emit(context.Background(), jobArgs{Job: "t1"}, crawl{})
	fmt.Println(wakeshift.IsCode(err, wakeshift.CodeBadArguments))
	// Output:
	// 0 <nil>
	// true
}

func ExampleEvent_Subscribe() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	finished, _ := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	ctx := context.Background()
	// Inside a tool that subscribes, call is its handler's (or
	// ToolCallFrom(ctx)). Replay also delivers an occurrence from the last
	// hour, for a job that finished before the subscribe.
	sub, err := finished.Subscribe(ctx, wakeshift.ToolCallFrom(ctx), jobArgs{Job: "3f2a"}, wakeshift.Once(), wakeshift.Replay(time.Hour))
	fmt.Println(sub.State, err)
	// Output: disabled <nil>
}

func ExampleClient_Attach() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{
		// Identify tells who's calling, from your own verified token, once
		// per HTTP request; RequireUser refuses anonymous callers.
		Identify: func(ctx context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
			if r.Token == nil {
				return wakeshift.Identity{}, nil
			}
			return wakeshift.Identity{User: r.Token.UserID}, nil
		},
		RequireUser: true,
	})
	next := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	// On a disabled client, Handler is next itself.
	fmt.Println(attached.Handler(next) == http.Handler(next))
	// Output: true
}

func ExampleAddTool() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	ciFinished, _ := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A CI run finished"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	// The handler is go-sdk's with the call after ctx; the input is your
	// own type. tools/list adds Wakeshift's arguments, and they're taken
	// out before your code sees them.
	wakeshift.AddTool(attached, &mcp.Tool{Name: "watch_ci", Description: "Watch a CI run."},
		func(ctx context.Context, call *wakeshift.ToolCall, in jobArgs) (*mcp.CallToolResult, startOut, error) {
			_, err := ciFinished.Subscribe(ctx, call, jobArgs{Job: in.Job})
			return nil, startOut{Job: in.Job}, err
		}, wakeshift.Subscribes(ciFinished))
	fmt.Println("registered")
	// Output: registered
}

func ExampleIsCode() {
	err := fmt.Errorf("emitting: %w", &wakeshift.APIError{Status: 409, Code: wakeshift.CodeIDReused, Message: "That id was used for another emit."})
	fmt.Println(wakeshift.IsCode(err, wakeshift.CodeIDReused))
	var apiErr *wakeshift.APIError
	fmt.Println(errors.As(err, &apiErr), apiErr.Status)
	// Output:
	// true
	// true 409
}
