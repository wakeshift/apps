# Wakeshift SDK for Go

[Wakeshift](https://wakeshift.ai) lets an MCP server wake the AI agents that use it. It wakes the agent that started the work when it finishes: your MCP server's tools subscribe the calling conversation to your app's events, and Wakeshift wakes it (Claude Code, Codex, ChatGPT and the rest) when your app emits one.

## Install

```sh
go get github.com/wakeshift/wakeshift-go@latest
```

```go
import "github.com/wakeshift/wakeshift-go"
```

## Quickstart

A crawler whose `start_crawl` tool wakes the conversation that started a crawl when it finishes. This is the whole integration for a server on the official Go MCP SDK:

```go
wake, err := wakeshift.New() // reads WAKESHIFT_KEY; without one, Wakeshift is off
if err != nil {
	log.Fatal(err) // a malformed key
}

type jobArgs struct {
	Job string `json:"job"`
}
type crawl struct {
	Status string `json:"status"`
	Pages  int    `json:"pages"`
}

crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
	Description: "A crawl finished",
})
if err != nil {
	log.Fatal(err)
}

srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
attached := wake.Attach(srv, wakeshift.AttachOptions{})

type startIn struct {
	URL string `json:"url"`
}
type startOut struct {
	Job string `json:"job"`
}

wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
	func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, startOut, error) {
		job := strings.ToLower(rand.Text()) // your tool minted the ID, so only this conversation can know it
		if _, err := crawlFinished.Subscribe(ctx, call, jobArgs{Job: job}, wakeshift.Once()); err != nil {
			return nil, startOut{}, err
		}
		go startCrawl(in.URL, job)
		return nil, startOut{Job: job}, nil
	}, wakeshift.Subscribes(crawlFinished))

h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
http.Handle("/mcp", attached.Handler(h))

// When the crawl finishes, in your worker or webhook handler:
onCrawlFinished := func(ctx context.Context, job string, pages int) error {
	_, err := crawlFinished.Emit(ctx, jobArgs{Job: job}, crawl{Status: "completed", Pages: pages})
	return err
}
```

- **The key comes from `WAKESHIFT_KEY`** (get one in the [console](https://app.wakeshift.ai) under **New app**). Without one, Wakeshift is off: every call still works, nothing is sent, and your server runs as before. `New` makes no network request.
- **`AddTool` is `mcp.AddTool` with the call after `ctx`**, and `Subscribes` names the events the tool subscribes to. Your input type stays your own: Wakeshift adds its arguments to `tools/list` and `attached.Handler` takes them out before go-sdk, your middleware or your code sees them.
- **Only your tools can subscribe to `crawl.finished`**, because it declares neither `Personal`, `Authorize` nor `Public`. That's the safe default for IDs your tool mints, and your tool minted this one, so there's nothing to check. A tool that subscribes to something the caller names checks the caller can see it first.
- **`Once` ends the subscription after one delivery**, and a call that fails ends what it subscribed.
- **`Emit` wakes every conversation subscribed with those arguments.** `Text` is optional: without it the agent reads the description and the arguments ("A crawl finished (job 3f2a…)").

Sign-in, webhooks, tools registered elsewhere, mcp-go, logging, errors and who may subscribe: the [Go guide](https://wakeshift.ai/docs/go).

## No webhook? Watch the job

When the service behind your tool can't call you back, check its status in the background and emit once it's done. Start the watch in the event's `OnSubscribe`, which runs once for each new subscription, however it was made (your tool, `subscribe_events`, a native client), with a `ctx` that outlives the request. Define `crawl.finished` with it, in place of the definition above:

```go
import "time"

crawlFinished, err = wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
	Description: "A crawl finished",
	OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[jobArgs]) error {
		go crawlFinished.Watch(ctx, s.Args, func(ctx context.Context) (crawl, bool, error) {
			done, pages, err := crawlStatus(ctx, s.Args.Job) // your service's own status call
			return crawl{Status: "completed", Pages: pages}, done, err
		}, wakeshift.Every(10*time.Second), wakeshift.Timeout(time.Hour))
		return nil
	},
})
```

It backs off on errors, stops at its timeout, emits with one ID across retries, and runs one watch per event and arguments. It lives in this process: a restart, a frozen serverless instance, or a stdio server's agent closing it ends it.

## Unit tests

`github.com/wakeshift/wakeshift-go/wakeshifttest` tests your tools offline: `wakeshifttest.NewClient()` is an enabled client answered by a fake Wakeshift in memory, recording what you emit and subscribe; `wakeshifttest.NewToolCall(…)` is a stand-in call for invoking a handler directly; `ContextWithCall` serves a tool named in `Wrap`. To keep a real client off in tests while `WAKESHIFT_KEY` is set, use `wakeshift.New(wakeshift.WithKey(""))`. With a key, tools/list and the capabilities gain Wakeshift's tools and arguments, so update any snapshot of them taken without one (`wakeshift.BuiltinToolNames()` names Wakeshift's tools).

## Compatibility

| | |
| --- | --- |
| Go | 1.25 or later |
| [go-sdk](https://github.com/modelcontextprotocol/go-sdk) | v1.8 or later |
| [mcp-go](https://github.com/mark3labs/mcp-go) | v1.0 or later, through `github.com/wakeshift/wakeshift-go/mcpgo` (adding the SDK keeps your version) |

`*Client`, `*Event` and `*Attachment` are safe for concurrent use. Create one `Client` per process. The same calls are in the [TypeScript](https://github.com/wakeshift/wakeshift-node) and [Python](https://github.com/wakeshift/wakeshift-python) SDKs. What changed between releases is in [CHANGELOG](CHANGELOG.md), and the package's reference is on [pkg.go.dev](https://pkg.go.dev/github.com/wakeshift/wakeshift-go).

## Testing

```sh
go test ./...
```

It runs the golden tests the TypeScript and Python SDKs share, against the files in `testdata/golden`. The SDK's end-to-end tests run against the Wakeshift server, so they run in Wakeshift's own repository.

## Contributing and security

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. Each commit here says which commit it came from (`.source-commit`); see [CONTRIBUTING](CONTRIBUTING.md). Report a security problem privately to support@wakeshift.ai, not in an issue ([SECURITY](SECURITY.md)).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
