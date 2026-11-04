# Wakeshift SDK for Go

Build apps that wake AI agents. Your app is an MCP server that offers events; agents (Claude Code, Codex, ChatGPT, Cursor and others) subscribe to them, and when your app emits one, [Wakeshift](https://wakeshift.ai) wakes every subscribed conversation, in whatever harness it runs.

Go 1.27 or later.

```sh
go get github.com/wakeshift/wakeshift-go@latest
```

```go
import "github.com/wakeshift/wakeshift-go/appkit"
```

## Get a key

In the [Wakeshift console](https://app.wakeshift.ai), open **Build → New app**. It gives you a key, `wst_…`: your app's private key. Keep it secret; Wakeshift keeps only the public half.

```go
wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "")
```

An empty address reads `WAKESHIFT_CORE`, then defaults to `https://app.wakeshift.ai`. `Open` asks Wakeshift once which app the key is (`wake.ID`). Older `wst_test_<app id>_…` keys still work; they need `WAKESHIFT_CORE`.

## Events

Define each event your app offers once, at startup: its name, the arguments a subscriber picks and what each occurrence carries (both Go types; their JSON Schemas come from them), who may subscribe, and the line it reads as.

```go
type channelArgs struct {
	Channel string `json:"channel" jsonschema:"The channel to watch"`
}
type message struct {
	From string `json:"from"`
	Text string `json:"text"`
}

msg, err := appkit.DefineEvent(wake, "chat.message", appkit.EventDef[channelArgs, message]{
	Description: "A new message in a channel",
	Authorize:   func(ctx context.Context, s appkit.Subscriber, a channelArgs) bool { return canRead(s.User, a.Channel) },
	Text:        func(m message, a channelArgs) string { return m.From + " in #" + a.Channel + ": " + m.Text },
})
```

Attach Wakeshift to your MCP server, and put it in front of the endpoint, after its authentication:

```go
attached := wake.Attach(srv, appkit.AttachOptions{
	Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
		return appkit.Identity{User: ti.UserID}, nil
	},
})
mux.Handle("/mcp", requireAuth(attached.Wrap(mcpHandler)))
```

Every client can now subscribe: ChatGPT with MCP Events, every other agent with the `subscribe_events` tool that `Attach` adds, and anyone can see and stop theirs with `list_subscriptions` and `unsubscribe_events`. (`AttachOptions.Tools` leaves out any of the three your own tools cover: `appkit.Tools{List: appkit.Tool{Disabled: true}}`.) With `Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}}`, in front of a stateless handler, `Wrap` also serves `wakeshift_wait` to clients that take MCP tasks (a recent Claude Code in the terminal): Wakeshift tells those conversations to call it, after the user declines Wakeshift's plugin, instead of starting a background monitor; other clients don't see it. It's off by default, because each waiting conversation then keeps a request open to your server (about 20 seconds at a time, the next a second later, for as long as it waits): give the endpoint a write timeout of at least 30 seconds. When something happens, emit it:

The three tools `Attach` adds, and `wakeshift_wait`, list MCP tool annotations: a title and all four hints, which app stores such as ChatGPT's ask for. `subscribe_events` ("Subscribe to events") and `unsubscribe_events` ("Unsubscribe from events") are idempotent, and not read-only, destructive or open-world: subscribing again renews the same subscription, unsubscribing again ends nothing, and an ended subscription comes back by subscribing. `list_subscriptions` ("List subscriptions") is read-only and idempotent. `wakeshift_wait` ("Wait for messages") is neither read-only nor idempotent, since each call confirms the messages the last one delivered; it isn't destructive or open-world either.

```go
msg.Emit(ctx, channelArgs{Channel: "eng"}, message{From: "ada", Text: "shipped it"})
```

Every conversation subscribed to `#eng` wakes with it, framed as information from outside. The SDK sends your app's manifest, with its events, automatically: it adds them to the ones Wakeshift has, and never removes one or clears a setting, so processes sharing your key keep each other's events. Once `Attach` has run, it's sent on the first call about events. Before then, and in a process that only emits, it's sent only when Wakeshift answers an emit with `unknown_event`, and the emit goes again: so you can emit at startup, or from a webhook, before any agent has connected (calling `PublishManifest` at startup works too, but you no longer need it). To set your app's settings, or to remove an event, call `wake.PublishManifest(ctx, appkit.Manifest{Description: "Team chat"})` (and `Agent`, `OthersCanWake`, `PreapproveTools`), which replaces the manifest: every later automatic send carries the settings you passed. `Manifest.Events` is only for events defined elsewhere, and listing a defined one is an error. A **personal** event (`Personal: true`) belongs to each subscriber: `Emit(…, appkit.To(user))` reaches that user's subscriptions only, so it can carry what only they may see ("it's your move: your cards are…").

## Subscribe from your own tools

A tool of yours can subscribe the conversation calling it, so one call (join a channel, take a seat, watch a repo) also sets up the wake-ups. Wrap its handler with `appkit.Events`, declare the events it subscribes to with `appkit.Subscribes`, embed `appkit.WakeArgs` in its input, and call `Subscribe`. Wakeshift adds what the agent needs to be woken to whatever the tool returns.

```go
type joinIn struct {
	Channel string `json:"channel"`
	appkit.WakeArgs // session_key, harness, conversation_id
}

mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join a channel"}, appkit.Events(attached,
	func(ctx context.Context, call *appkit.ToolCall, in joinIn) (*mcp.CallToolResult, any, error) {
		// Your own access check first: can call.User read in.Channel?
		sub, err := msg.Subscribe(ctx, call, channelArgs{Channel: in.Channel}, appkit.Reason("In #"+in.Channel))
		if err != nil {
			return nil, nil, err
		}
		members.Add(in.Channel, sub.User, sub.ID) // to reach this conversation later
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Joined #" + in.Channel + "."}}}, nil, nil
	}, appkit.Subscribes(msg)))
```

`appkit.Subscribes` makes tools/list add a sentence to the tool's description, built from each event's own description: "This tool is how Chat notifies you in this conversation of this event: A new message in a channel. Chat delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." Agents set aside what Wakeshift adds to a result as something the user didn't ask for unless the tool's description says the tool notifies through it, so declare every event a tool subscribes to. The app is named as the console has it. A `Subscribe` to an event the tool doesn't declare still works but logs a warning, once per tool, with the fix; from the next release it returns an error. go-sdk doesn't tell `Events` the tool's name, so tools/list finds the tool by the input schema its input type gives it: give each subscribing tool its own input type, or name it with `appkit.ToolName("join")`. To word the sentence yourself, write the description with `appkit.Notifies` (the tool then gets no second sentence).

Event descriptions are read by agents in these tool descriptions, so write each as a plain statement of what happened ("A new message in a channel."), not as instructions to the agent.

`Unsubscribe` ends it, and a call that fails ends what it subscribed. An argument a tool doesn't declare is ignored, not refused: on a server you attached, the SDK drops it before the MCP SDK checks the input, for every tool. Your app keeps its own membership, and reaches the conversations it stored in one emit, by user, by subscription, or natively, up to 1,000 at a time:

```go
note.Emit(ctx, noteArgs{}, notification{From: "ada", Text: "@bo look"},
	appkit.ToSubscription(bo.User, bo.ID), appkit.ToSubscription(cy.User, cy.ID), appkit.ToNative("dee"))
```

`Status` and `StatusOf` say who is subscribed and connected now; `PublishURL` makes a URL that emits an event from a service's webhook.

## mcp-go

A server built with [mcp-go](https://github.com/mark3labs/mcp-go) (v1.5 or later) attaches with `appkit/mcpgo`: `mcpgo.Attach(wake, srv, opts)` and `mcpgo.Events(attached, tool, handler, appkit.Subscribes(…))`, which returns a `server.ServerTool` for `srv.AddTools`. Events, `Subscribe` and `Emit` are the same `appkit` calls. An app on the official SDK never builds mcp-go.

```go mcp-go
srv := server.NewMCPServer("chat", "1.0.0")
attached := mcpgo.Attach(wake, srv, mcpgo.AttachOptions{
	Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
		if ti == nil {
			return appkit.Identity{}, nil
		}
		return appkit.Identity{User: ti.UserID}, nil
	},
})
srv.AddTools(mcpgo.Events(attached, mcp.NewTool("join", mcp.WithDescription("Join a channel"), mcp.WithString("channel", mcp.Required())),
	func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, error) {
		if _, err := msg.Subscribe(ctx, call, channelArgs{Channel: in.Channel}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Joined #" + in.Channel + "."), nil
	}, appkit.Subscribes(msg)))
mux.Handle("/mcp", requireAuth(attached.Wrap(server.NewStreamableHTTPServer(srv))))
```

`Events` adds `session_key`, `harness` and `conversation_id` to the tool's input schema for you. `Identify` runs for every request, with the token `appkit.RequireBearer` verified or `nil`, so an app that authenticates another way reads who it is from `ctx`.

## Sign-in

Your app identifies its own users, with its own sign-in; Wakeshift never signs them in for you. Return your user's ID from `AttachOptions.Identify`, and Wakeshift keeps a record per user for your app. On a stateless server, which never sees `initialize`, also return the MCP client's name as your sign-in knows it (`Identity.Client`, an OAuth registration's `client_name`, say), so Wakeshift can tell which program the agent runs in; a name the request carries wins. Users never see Wakeshift. To act for a user's agent, `Workspace` and `AddAgent` set it up, `ActAs` acts for it, `CheckAgent` says whether its grant is still live, and `HarnessAgent` names the agent after its MCP client (`claude-code`). `RequireBearer` checks the tokens your sign-in issues on your MCP endpoint, and `ForgetUser` deletes what Wakeshift keeps for a user.

## Errors

A refusal is an `*appkit.APIError`, with `Status`, `Code` and `Message`, and `appkit.IsCode(err, code)` checks one. Mistakes the SDK catches before sending (an emit without a recipient, another app's event) get the code Wakeshift gives for the same mistake, as in the TypeScript and Python SDKs:

| Status | Code | When |
| --- | --- | --- |
| `400` | `bad_request` | Something in the request is invalid, including a field the API doesn't take. The message says what. Also more than 1,000 subscriptions in one `StatusOf`, or one without its user. |
| `400` | `bad_arguments` | An event's arguments or payload don't match its schemas, a routing value isn't a lowercase ID, or the recipients don't fit the event: a personal event's emit, publish URL or status without `To` (a publish URL's or status's names exactly one user), a shared event's with one, a recipient with an empty user or subscription, or `Subscribe` on a personal event with no user. |
| `401` | `unauthenticated` | The assertion was refused: expired, reused, signed with another key, or for another `aud` (a key for another Wakeshift). The message says which. |
| `403` | `other_client` | The `session_key` is a conversation in another MCP client of the same user. Call again without it. |
| `404` | `unknown_event` | Your app offers no event by that name (an emit gets this only once the SDK has sent your manifest and tried again), or `Subscribe` or `Unsubscribe` was given another app's event (one defined on another `*appkit.App`). |
| `404` | `unknown_publish_url` | The publish URL ID isn't one of your app's. |
| `409` | `id_reused` | An emit's `id` was used within the last day for a different emit (another event, arguments, recipients or payload). Nothing was sent. Give each occurrence its own `id`. |
| `413` | `too_large` | The request body is over 64 KB (512 KB for an emit or a status call, room for 1,000 recipients). |
| `429` | `rate_limited` | Your emits to lists named over 10,000 recipients in a minute. |
| `0` | `unreachable` | No answer at all: a wrong core, a network problem, or the HTTP client's timeout. The message names the core it tried, and the error unwraps to the transport's. |

## Examples

[wakeshift/apps](https://github.com/wakeshift/apps) has complete apps built on this SDK: multiplayer games for agents (Secret Agent, Nightfall), a chat app (Agent Chat) and a Slack bridge (Slackline).

The same calls are in the [TypeScript](https://github.com/wakeshift/wakeshift-node) and [Python](https://github.com/wakeshift/wakeshift-python) SDKs. What changed between releases is in [CHANGELOG](CHANGELOG.md).

## Documentation

The guides and the full reference are at [wakeshift.ai/docs](https://wakeshift.ai/docs): the [quickstart](https://wakeshift.ai/docs/quickstart), [events](https://wakeshift.ai/docs/events), [your users](https://wakeshift.ai/docs/users) and [the Go SDK](https://wakeshift.ai/docs/go). The package's reference is on [pkg.go.dev](https://pkg.go.dev/github.com/wakeshift/wakeshift-go/appkit).

## Testing

```sh
go test ./...
```

It runs the golden tests the TypeScript and Python SDKs share, against the files in `appkit/testdata/golden`. The SDK's end-to-end tests run against the Wakeshift server, so they run in Wakeshift's own repository.

## Contributing and security

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. Each commit here says which commit it came from (`.source-commit`); see [CONTRIBUTING](CONTRIBUTING.md). Report a security problem privately to support@wakeshift.ai, not in an issue ([SECURITY](SECURITY.md)).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
