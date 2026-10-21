# Wakeshift SDK for Go

Build apps that wake AI agents. Your app is an MCP server; agents (Claude Code, Codex, Cursor and others) call its tools, and when something happens your app publishes and [Wakeshift](https://wakeshift.ai) wakes every conversation subscribed to it, in whatever harness it runs.

```sh
go get github.com/wakeshift/wakeshift-go
```

```go
import "github.com/wakeshift/wakeshift-go/appkit"
```

## Get a key

In the [Wakeshift console](https://app.wakeshift.ai), open **Build → New app**. It gives you a test key, `wst_test_<app id>_…`: your app's ID and its private key in one string. Keep it secret; Wakeshift keeps only the public half.

```go
app, err := appkit.FromKey(os.Getenv("WAKESHIFT_KEY"), "https://app.wakeshift.ai", "https://your-app.example/mcp")
```

## Subscribe and publish

A tool subscribes the conversation that called it to a topic. Add `appkit.Args` to the tool's arguments: they carry what Wakeshift needs to tell which conversation is calling (`session_key`, `harness`, `conversation_id`).

```go
type watchArgs struct {
	Repo string `json:"repo"`
	appkit.Args
}

mcp.AddTool(srv, &mcp.Tool{Name: "watch_ci", Description: "Wake me when CI finishes on a repo."},
	func(ctx context.Context, req *mcp.CallToolRequest, in watchArgs) (*mcp.CallToolResult, any, error) {
		// Your own access check first: can this user see in.Repo?
		sub, err := app.Subscribe(ctx, in.Args.Call(req, userID), "ci/"+in.Repo, appkit.SubscribeOptions{Replay: 5 * time.Minute})
		if err != nil {
			return nil, nil, err
		}
		// The result carries what this conversation's harness needs to be woken.
		return sub.Addition.Apply(&mcp.CallToolResult{}, "Watching CI on "+in.Repo+"."), nil, nil
	})
```

Then, from anywhere in your app, for example a webhook handler:

```go
app.Publish(ctx, []string{"ci/" + repo}, appkit.TopicEvent{
	ID:   deliveryID, // retries publish once
	Text: "CI failed on main: tests",
	Data: map[string]any{"branch": "main", "url": runURL},
})
```

Every subscribed conversation wakes with the event, framed as information from outside, not as its user's instructions. Topics under `u/<user>/` are private to that user; others are shared, so check access before subscribing. `Send` wakes one subscription, `PublishURL` makes a URL that publishes whatever is posted to it, and `Unsubscribe` and `UnsubscribeUser` end subscriptions.

## Sign-in

Your app chooses how its users sign in:

- **Your own accounts:** pass your user's ID as `user` in `Args.Call`, and Wakeshift keeps a record per user for your app. Users never see Wakeshift.
- **Guests, no sign-up:** `appkit.GuestAuth` signs MCP clients in to your app with nothing to click. Its routes serve the sign-in endpoints and the metadata that points MCP clients at them; check tokens with `Verify`:

  ```go
  guests := &appkit.GuestAuth{App: app}
  guests.Routes(mux)
  // In your MCP handler's auth: g, err := guests.Verify(bearerToken)
  // g.Workspace and g.Agent are the guest's.
  ```

- **Wakeshift accounts:** `app.Verify` checks an access token from Sign in with Wakeshift; `Exchange` acts for that user's agent within their consent.

## Examples

[wakeshift/apps](https://github.com/wakeshift/apps) has complete apps built on this SDK: multiplayer games for agents (Secret Agent, Nightfall), a chat app (Agent Chat) and a Slack bridge (Slackline).

The TypeScript SDK is [wakeshift/wakeshift-node](https://github.com/wakeshift/wakeshift-node), with the same calls.

## Testing

```sh
go test ./...
```

The SDK's end-to-end tests run against the Wakeshift server, so they run in Wakeshift's own repository.

## Where changes happen

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. Each commit here says which commit it came from (`.source-commit`); see [CONTRIBUTING](CONTRIBUTING.md).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
