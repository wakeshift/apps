package mcpgo_test

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/mcpgo"
)

type repoArgs struct {
	Repo string `json:"repo"`
}

type ciRun struct {
	Status string `json:"status"`
}

type watchIn struct {
	Repo string `json:"repo"`
}

// The quickstart on mcp-go, on a disabled client (no key): offline and the
// same every time.
func Example() {
	wake, err := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		log.Fatal(err)
	}
	ciFinished, err := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[repoArgs, ciRun]{Description: "A CI run finished"})
	if err != nil {
		log.Fatal(err)
	}
	srv := server.NewMCPServer("ci", "1.0.0")
	attached := mcpgo.Attach(wake, srv, wakeshift.AttachOptions{})
	mcpgo.AddTool(attached, mcp.NewTool("watch_ci", mcp.WithDescription("Watch CI on a repo."), mcp.WithString("repo", mcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, error) {
			if _, err := ciFinished.Subscribe(ctx, call, repoArgs{Repo: in.Repo}); err != nil {
				return nil, err
			}
			return mcp.NewToolResultText("Watching " + in.Repo + "."), nil
		}, wakeshift.Subscribes(ciFinished))
	http.Handle("/example/mcp", attached.Handler(server.NewStreamableHTTPServer(srv)))

	got, err := ciFinished.Emit(context.Background(), repoArgs{Repo: "acme/api"}, ciRun{Status: "passed"})
	fmt.Println(got.Matched, err)
	// Output: 0 <nil>
}

func ExampleAddTool() {
	wake, _ := wakeshift.New(wakeshift.WithKey(""), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	ciFinished, _ := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[repoArgs, ciRun]{Description: "A CI run finished"})
	srv := server.NewMCPServer("ci", "1.0.0")
	attached := mcpgo.Attach(wake, srv, wakeshift.AttachOptions{})
	// The handler gets the call, mcp-go's request and your own input type;
	// Wakeshift's arguments never reach it.
	mcpgo.AddTool(attached, mcp.NewTool("watch_ci", mcp.WithString("repo", mcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, error) {
			sub, err := ciFinished.Subscribe(ctx, call, repoArgs{Repo: in.Repo})
			if err != nil {
				return nil, err
			}
			return mcp.NewToolResultText(string(sub.State)), nil
		}, wakeshift.Subscribes(ciFinished))
	fmt.Println(srv.GetTool("watch_ci") != nil)
	// Output: true
}
