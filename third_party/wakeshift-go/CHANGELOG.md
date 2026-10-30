# Changelog

What changed in each release of `github.com/wakeshift/wakeshift-go`. v0.4.0 is the first published release: to start with it, read the [README](README.md). Its notes below, and the earlier releases', are for apps that ran the preview builds.

## v0.5.2 (2026-10-30)

### Added

- **Claude Code is woken with nothing to install, and no restarts.** In front of a stateless handler (`mcp.StreamableHTTPOptions{Stateless: true}`), `Attachment.Wrap` now serves `wakeshift_wait` to clients that take MCP tasks: a recent Claude Code in the terminal, where Anthropic has turned them on. Wakeshift tells those conversations to call it after they subscribe; it returns at once and waits in the background, through `/compact` and restarts of Claude Code, and wakes the conversation when your events arrive. Other clients don't see it, and keep the instructions they had. Nothing to change in your code.

### Fixed

- **Agents that see only `structuredContent` are told how they're woken.** Some hosts, Claude Code among them, show the model a tool's `structuredContent` and not its text. A wrapped tool with a typed output that didn't embed `appkit.WakeOutput` left Wakeshift's instructions out of it, so an agent could say it was watching and never be woken. Now every structured result of a wrapped tool carries them in its `wakeshift` field, and `tools/list` adds that optional field to the tool's output schema (go-sdk's inferred schemas refuse fields they don't name, and clients that check results against the schema would refuse the result). Embedding `appkit.WakeOutput` still declares the field in your type; it's no longer required. Nothing to change in your code.

## v0.5.1 (2026-10-30)

### Added

- **Clients that speak MCP Events can poll.** `Attach` now answers `events/poll`, and `events/list` offers `poll` beside `webhook` for every event. A client that can't take webhooks (an agent with no public URL, or Mastra in its poll mode) subscribes with its first poll, then each poll returns what happened since its cursor, in order. A poll's cursor acknowledges everything before it, so an event isn't lost when a poll fails: the next poll returns it again. Your `Authorize` runs on every poll. Wakeshift keeps the subscription while the client polls (it lapses after 10 minutes without a poll), lists it with the user's other subscriptions (`list_subscriptions` says "polled by an MCP client"), and answers the client's next poll with `Forbidden` once the user stops it. Nothing to change in your code.
- **Webhooks for an event you no longer offer end.** When you remove an event from your app's manifest (with `PublishManifest`), Wakeshift ends each webhook subscription to it within an hour, telling the client the event is gone (`terminated`, with `NotFound` and `data.kind: "event"`), so the client lists your events again rather than waiting for its next refresh. A poll for it gets `NotFound` at once. Nothing to change in your code.

### Changed

- `Attach` follows SEP-3415 as revised on 2026-10-27. `ResourceExhausted`'s data is now `{"limit": "subscriptions"}`, the draft's name for it (it was `{"kind": "subscriptions"}`). `events/stream` still answers `Unsupported`: push isn't offered.

## v0.5.0 (2026-10-29)

### Breaking

- **`Recognised` is now `Recognized`.** The type `appkit.Recognised` is `appkit.Recognized`, and the fields `ToolCall.Recognised` and `Subscription.Recognised` are `Recognized`; `Subscription` marshals it to JSON as `recognized`. Rename them where you use them: `sub.Recognised.Harness` becomes `sub.Recognized.Harness`. What they hold is unchanged.

### Fixed

- **Processes that share your key no longer remove each other's events.** In v0.4.0 every process sent the event types it defined as your app's whole list. With an MCP server and a separate emitter on one key, whichever sent last removed the other's events, and the other's emits failed with `unknown_event` until it restarted. Now only your MCP server, the process that calls `Attach`, sends the manifest automatically, and Wakeshift merges it into the one it has: it adds or updates your event types and the settings you passed to `PublishManifest`, and never removes an event or clears a setting. A process that doesn't call `Attach` never sends it, whatever it calls (`Emit`, `Status`). `PublishManifest` still replaces the whole manifest, and is how you remove an event.
- A process with no events defined never sends the manifest. In v0.4.0 an app call from one about events sent an empty list, which removed every one of your app's events.
- If an event is defined only in a process that emits it, define it in your MCP server too (or publish it with `PublishManifest`): until a server sends it, emitting it fails with `unknown_event`.

## v0.4.0 (2026-10-29)

Define your app's events (`DefineEvent`), attach Wakeshift to your MCP server (`Attach`), subscribe the conversations that call your own tools (`appkit.Events`, `event.Subscribe`), and emit to them (`Emit`). The rest of this section is what changed since v0.3.0.

### Breaking

**One way to subscribe from your own tools.** Wrap a tool's handler with `appkit.Events`, embed `appkit.WakeArgs` in its input, and subscribe the calling conversation with `event.Subscribe(ctx, call, args)`. Wakeshift adds what the agent needs to be woken to whatever the handler returns, once per call, so a conversation can't be left unwakeable by a forgotten argument, a wrong tool name or a result you didn't pass on.

A join tool, before:

```go
type joinIn struct {
	Table string `json:"table"`
	appkit.Args
}

mcp.AddTool(srv, &mcp.Tool{Name: "join"}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, any, error) {
	sub, err := turn.Subscribe(ctx, in.Args.Call(req, userOf(req)), tableArgs{Table: in.Table})
	if err != nil {
		return nil, nil, err
	}
	return sub.Addition.Apply(&mcp.CallToolResult{}, "Seated at "+in.Table+"."), nil, nil
})
```

After:

```go
type joinIn struct {
	Table string `json:"table"`
	appkit.WakeArgs
}

attached := wake.Attach(srv, appkit.AttachOptions{Identify: identify}) // before the tools
mcp.AddTool(srv, &mcp.Tool{Name: "join"}, appkit.Events(attached,
	func(ctx context.Context, call *appkit.ToolCall, in joinIn) (*mcp.CallToolResult, any, error) {
		sub, err := turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, appkit.Reason("Playing at "+in.Table))
		if err != nil {
			return nil, nil, err
		}
		seats.Add(in.Table, sub.User, sub.ID) // to reach this conversation later
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated at " + in.Table + "."}}}, nil, nil
	}))
```

A tool with structured output embeds `appkit.WakeOutput` in its output, and gets Wakeshift's text in `structuredContent` too (before: `WatchCall` and `w.Result(out)`):

```go
type view struct {
	Table string `json:"table"`
	appkit.WakeOutput
}
```

A leave tool, before: `turn.Unsubscribe(ctx, in.Args.Call(req, user), args)`. After, in a wrapped tool: `turn.Unsubscribe(ctx, call, args)`.

Topics are gone: use events (`DefineEvent`, `event.Subscribe` from a wrapped tool, `Emit`, `Status`, `PublishURL`, `End`). Before: `app.Publish(ctx, []string{"ci/acme/api"}, appkit.TopicEvent{Text: "CI failed"})`; after: `ciFinished.Emit(ctx, repoArgs{Owner: "acme", Repo: "api"}, run)`. Before: `app.TopicStatus(ctx, topic)`; after: `ciFinished.Status(ctx, args)`.

Groups are gone: keep membership in your app (a channel's members, a game's seats), subscribe each conversation from a wrapped tool, and emit to your members' subscriptions (`Emit` with `ToSubscription`).

**Mistakes caught before asking Wakeshift are APIErrors.** `Emit`, `PublishURL`, `Status`, `StatusOf`, `Subscribe` and `Unsubscribe` return an `*APIError` for a mistake they catch before asking Wakeshift, with the status and code Wakeshift gives for the same mistake, so `appkit.IsCode` works on them as it does in the TypeScript and Python SDKs. They were plain errors starting `appkit:`; they print `wakeshift: <code> (<status>): …`.

- `bad_arguments` (400): a personal event's `Emit` without a recipient, or `PublishURL` or `Status` without exactly one user in `To`; a shared event's with one; a recipient with an empty user or subscription; `Subscribe` on a personal event with no user.
- `bad_request` (400): `StatusOf` with more than 1,000 subscriptions, or one without its user or ID.
- `unknown_event` (404): `Subscribe` or `Unsubscribe` given another `App`'s event.

**A core that doesn't answer is an APIError.** A wrong address, a network problem or the HTTP client's timeout is an `*APIError` with `Status` 0 and the code `unreachable`, whose message names the core it tried, as in the TypeScript and Python SDKs. It was the transport's plain error; the `APIError` unwraps to it, so `errors.Is(err, context.DeadlineExceeded)` still works.

**`Open` takes the key and the core.** `appkit.Open(ctx, key, core)`: the MCP endpoint's URL it took was never used.

```go
// before
wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "", "https://your-app.example/mcp")

// now
wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "")
```

**A `Filter` may not name a field an optional argument filters.** Without a `Topic`, `DefineEvent` adds a filter for each optional argument to `EventDef.Filter`; where both named the same payload field, the argument's entry silently replaced yours. Now `DefineEvent` returns an error, naming the field: remove it from `Filter`, or set `Topic` and `Filter` both.

### Removed

- What only Wakeshift's own apps used: an agent's sessions and stream links (`Agent.SessionLink`, `StreamLink`, `Sessions`, `ConversationSession`, `Unwatch`, `UnwatchNote`, `Session`, `Connector`, `Install`, `Link`, `LinkRequest`), pull delivery (`Agent.Wait`, `Ack`, `Seen`, `Inbox`, `WaitTool`, `InboxNotes`, `InboxNote`, `AllTools`, `DefaultFormat`, `Delivery`, `Event`, `Party`, `WaitRequest`, `Waited`, the wait notes, `MaxWaitSeconds`, `DefaultWaitSeconds`), and `Load` and `FromKey`. `Open` reads an older key (`wst_test_…`, `wst_live_…`) without calling Wakeshift, as `FromKey` did. They live on in [wakeshift/apps](https://github.com/wakeshift/apps)' `agentkit`, built on `Agent.Request`.
- `DefinedEvent.Subscribe` and `Unsubscribe` taking a `Call`, and `CallerSubscription` (`Addition`): `Subscribe` and `Unsubscribe` take the wrapped tool's `*ToolCall` and return a `Subscription`.
- Topics: `App.Subscribe`, `Unsubscribe`, `Publish`, `Send`, `TopicStatus`, `SubscriptionStatus`, `PublishURL(topic)`, `CloseTopic`, `EndSubscription`, `UnsubscribeUser`, `Annotate`, and `TopicSubscription`, `TopicEvent`, `Published`, `TopicStatus`, `SubscriptionStatus`, `SubscribeOptions`, `SendDelivered`, `SendQueued`, `SendClosed`.
- `Call`, `CallFrom`, `Args` and `Args.Call`, `Addition` and `Addition.Apply`: the wrapper packs the call and adds the block.
- Groups: `CreateGroup`, `App.Group`, `RemoveMember`, `CloseGroup`, `PostToGroup`, `Group`, `GroupMember`, `GroupPost`, `Sender`, `Posted`, `EventDef.Group`, `GroupMessage`, `ManifestEvent.Group` and `AgentScope.Groups`. Set `Manifest.OthersCanWake` if other people and their agents wake your users' agents through your app.
- `Agent.SubscribeAs`, `Membership`, `SubscribeAgentWide`, `SubscribeAgentWideWhere`, `Agent.Unsubscribe`, `UnsubscribeSession`, `Agent.Call`, `Publish`, `Post`, `History`, `Groups`, `DeclareEventType`, `EventTypes`, `EventType`, `Member`.
- `Agent.WatchCall`, `WatchedCall`, `WatchArgs`, `WatchRequest`, `Watch`, `WatchFor`, `Beacon`, `BeaconFor`, `Watched`, `ConnectorMeta`, `CodexWatchNote`, `AnnotateCall`, `AgentBeacons` and `ConnectorBeacons`: `event.Subscribe` and `call.ResolveSession` in a wrapped tool, and the block it adds.
- `ParseAppKey`, whose mode meant nothing since current keys came in: use `Open` (either format), or `ParseKey` for the private key. Older keys (`wst_test_…`, `wst_live_…`) still work and name their app; a `wst_test_` key still needs a core.
- `session` as a hidden alias of `session_key`: a wrapped tool takes `session_key` only, and ignores a `session` argument like any other its input doesn't name.
- Reading a client's top-level `events` capability, the earlier MCP Events sketch's key: a client speaks MCP Events if it declares the extension (`extensions["io.modelcontextprotocol/events"]`) or is ChatGPT. `Attach` still advertises both keys, since ChatGPT reads the top-level one.
- `PublishURL.Topic`: a publish URL is an event's (`PublishURL.Event`).
- `SpeaksEvents`: a wrapped tool's `call.Native` says whether its caller subscribes to your events itself.

Kept: `DefineEvent`, `Emit`, `PublishURL`, `End`, `Attach` and its three tools and native MCP Events, `ForgetUser`, `Workspace`, `AddAgent`, `ActAs` and `CheckAgent`.

### Added

- `appkit.Events(attached, handler)` wraps a tool handler for `mcp.AddTool`: the handler gets a `*ToolCall` (`User`, `Agent`, `Tool`, `Native`, `NativeConversation`, `Session`, `Recognised`), and subscribes the calling conversation with `event.Subscribe(ctx, call, args, appkit.TTL(…), appkit.Reason(…), appkit.Label(…))`, ends it with `event.Unsubscribe`, or finds its session with `call.ResolveSession` (without subscribing it). A `ToolCall` that `Events` didn't make returns `ErrNoCall`; one used after its handler returned, `ErrCallEnded`.
- `appkit.WakeArgs` (embed it in a wrapped tool's input: `session_key`, `harness`, `conversation_id`) and `appkit.WakeOutput` (embed it in a tool's output to get the block's text in `structuredContent` too).
- `Identity.Client`: the MCP client's name, for a stateless server that never sees `initialize`. Wakeshift reads it to tell which program the agent runs in, and whether the client subscribes to events itself. A name the request carries (its `_meta`'s, or `initialize`'s) wins.
- `AttachOptions.Tools` names which of `Attach`'s own tools it adds (`SubscribeEventsTool`, `UnsubscribeEventsTool`, `ListSubscriptionsTool`). Nil adds all three; an empty list adds none.
- `AttachOptions.Events.Native` tells `Events` that a call comes from a client that subscribes itself; the SDK also recognizes ChatGPT's `_meta["openai/session"]`, its client name or User-Agent, and clients that declare MCP Events, on stateless servers too.
- `Emit` to several recipients in one call: `To(users...)`, `ToSubscription(user, id)` and `ToNative(users...)`, mixable, at most 1,000. `Emitted` gains `Unmatched` and `Ended`.
- `event.Status(ctx, args, appkit.To(user))`: how many conversations are subscribed to an event with these arguments (a personal event: one user's), how many are connected now, and how. `event.StatusOf(ctx, subs...)` reads up to 1,000 subscriptions by ID (each a `Recipient` with its user and subscription): whether each is live and its conversation connected.
- A `Reason` on `Subscribe`, shown in the console.
- `Agent.Request(ctx, method, path, body, out)`: a request to Wakeshift's app API as an agent from `ActAs`, for the agent routes the SDK doesn't wrap. A deadline on `ctx` bounds it in place of the HTTP client's timeout, so a long poll can outlast it.
- `appkit.ToolCallFrom(ctx)`: the wrapped tool's `*ToolCall` a request belongs to, for receiving middleware added before `Attach` (nil for a tool `Events` didn't wrap).
- `App.CheckAgent(ctx, workspace, agent)`: whether an agent's grant is still live (`no_grant` once it's revoked, `unknown_workspace` once the user is forgotten), as the other SDKs' `checkAgent` does.
- `ViaWebhook`, the `Via` of a native subscription.
- `Manifest.OthersCanWake`: your app wakes its users' agents with what other people and their agents do there, so the consent screen says agents of people they don't know can wake theirs.
- `Manifest.PreapproveTools`: your tools, by name, that agents' programs can run without asking each time (at most 20). When a user connects Claude Code or Cursor CLI, Wakeshift offers to allow exactly these, in the same question as its install.

### Changed

- A wrapped tool's reply carries one Wakeshift block, however many subscriptions it makes, and only when it subscribed (or Wakeshift has something for the conversation).
- A subscription a failed tool call made is ended: an error, an `isError` result, an output the schema refuses, a JSON-RPC error, a panic or a canceled request.
- A client that speaks MCP Events is told which event to subscribe to, or that it has.
- `session_key` is the one name for the session argument.
- An argument a tool's input doesn't declare is ignored, not refused, as in the TypeScript and Python SDKs: on a server you `Attach` to, the SDK drops it before the MCP SDK checks the input, for every tool, wrapped or not (`session`, the session argument's old name, included). A schema that allows other properties keeps them.
- `Attach`'s own tools (`subscribe_events`, `unsubscribe_events`, `list_subscriptions`) describe `session_key` to the agent as a wrapped tool's `WakeArgs` does.
- `list_subscriptions` is marked read-only and idempotent (`readOnlyHint` and `idempotentHint` true), as in the TypeScript and Python SDKs; it said `idempotentHint: false`.
- `Attach` panics when no event is defined, as the TypeScript and Python SDKs' `attach` throw: it would serve nothing. Define your events first.
- You needn't call `PublishManifest`. Wakeshift gets the manifest automatically the first time your app needs it (an emit, a status, a publish URL, a subscription), and again after a `DefineEvent`, as the TypeScript and Python SDKs do. The automatic send carries everything your app configured: the manifest you last passed to `PublishManifest` (even if that call failed), or else the `Description`, `Agent`, `OthersCanWake` and `PreapproveTools` Wakeshift already has, so it never wipes them. A manifest's `Events` still go before the defined ones, and an event both defined and listed is an error.

### Fixed

- `DefineEvent` checks an `EventDef.Filter` given without a `Topic` as it checks one given with it: an entry on a field the payload doesn't have, or naming an argument the event doesn't take, is an error at `DefineEvent`, not when the manifest is sent.
- Recipients are trimmed of the whitespace around them before they're sent, as in every SDK: an emit's users and subscriptions, `PublishURL`'s user, and `StatusOf`'s users and subscription IDs. One that's empty once trimmed is refused, as before.
- An `APIError` without Wakeshift's message (a proxy's page, an empty answer) has the body's first 200 bytes as its `Message`, else the status's text, and prints `error` for an empty code, as the Node and Python SDKs do.
- A successful answer that isn't a JSON object (a proxy's page, a wrong core) is an `APIError` with the code `bad_response`, not a JSON decoding error.
- `ToSubscription(user, "")` is refused (`bad_arguments`) instead of reaching every subscription the user has; `Status` and `PublishURL` refuse it too.
- `EventDef.Filter` without a `Topic` was dropped. It's kept, with the default route's filter on each optional argument added to it, as in the other SDKs.
- `Subscription.User` is the caller's user when Wakeshift's answer doesn't name one.
- A failed tool call's rollback no longer ends a subscription that another call on the same conversation renewed in the meantime (two calls in parallel, or a second call while the first was still running). The call that renewed it told the agent it would be woken; now it is.

## v0.3.0 (2026-10-26)

### Breaking: guest sign-in is removed

`GuestAuth` and `Guest` are gone: your app signs its own users in, with its own sign-in, and tells Wakeshift who they are (`Args.Call`'s `user`, or `AttachOptions.Identify`). Apps on v0.2 that use `GuestAuth` must replace it with their own sign-in before upgrading. Unchanged: `Workspace`, `AddAgent`, `ActAs`, `HarnessAgent`, `RequireBearer`, `TokenError`, `RateLimiter`, `ForgetUser`, topics, groups and events.

Wakeshift also no longer forgets an app's idle users by their ID's `guest:` prefix: an app forgets the users it no longer needs with `ForgetUser`. Workspaces made by v0.2's `GuestAuth` are still forgotten once unused for 31 days.

### Added

- `Call.Tool`: the name of the tool being called, which `CallFrom` and `Args.Call` set from the request. The connector beacon Wakeshift adds to a result names that tool, and Wakeshift's hooks act on it only from that tool's result, so apply an `Addition` to the result of the tool its call came from.
- `Agent.WatchFor` and `Agent.BeaconFor`: `Watch` and `Beacon` for the result of a named tool (`req.Params.Name`). `ConnectorBeacons` names the tool whose result it adds a beacon to.

### Changed

- An app's user has at most 20 live MCP Events webhook subscriptions. Wakeshift refuses one more with `403 subscription_limit`, which `Attach` answers with MCP Events' `ResourceExhausted` (`InvalidParams` for ChatGPT), with data `{"kind": "subscriptions"}`.

## v0.2.0 (2026-10-26)

### Breaking: Sign in with Wakeshift for apps is removed

Every app now identifies its own users, with its own sign-in or with guest sign-in (`GuestAuth`), and acts only in the workspaces it owns. Wakeshift no longer gives apps access to people's own Wakeshift workspaces, so the calls for it are gone. Compared with v0.1.0, these are removed:

- `App.Exchange`, which turned a Wakeshift access token into the agent it signed in.
- `App.Verify` and `Claims`, which checked an access token from Sign in with Wakeshift. `GuestAuth.Verify` is unchanged.
- `App.ResourceMetadata`, `App.Challenge` and `App.RefuseToken`, which pointed MCP clients at Wakeshift to sign in. `GuestAuth` serves its own metadata.
- `App.AuthorizeURL`, `App.SignIn`, `App.Refresh`, `PKCE` and `SignedIn`: Sign in with Wakeshift from your app's own sign-in.
- `App.Redeem`, `Connection`, `App.PublishToWorkspace`, `SourceEvent` and `WorkspaceScope`: publishing into people's own workspaces.
- `Manifest.Accounts`, `Manifest.Workspace` and `AgentScope.RedirectURIs`. Wakeshift refuses a manifest that asks for `accounts: "wakeshift"`, a workspace scope or redirect URIs.
- `Agent.Expires`.

Unchanged: `GuestAuth`, workspaces your app owns and `ActAs`, topics, groups, and events (`DefineEvent`, `Emit`).

### Breaking: `ServeEvents` is now `Attach`

`App.ServeEvents(srv, EventsOptions)` is renamed `App.Attach(srv, AttachOptions)`, the one call that attaches Wakeshift to your MCP server. It returns an `*Attachment`, whose `Wrap(handler)` is the middleware `ServeEvents` returned. `EventsOptions.User` and `EventsOptions.Agent` become one `AttachOptions.Identify(ctx, tokenInfo)`, which returns an `Identity` (`User`, and `Agent` for an agent from `ActAs`); an error counts as no user. It behaves as before:

```go
// before
events := app.ServeEvents(srv, appkit.EventsOptions{User: func(ti *auth.TokenInfo) string { return ti.UserID }})
mux.Handle("/mcp", requireAuth(events(h)))

// now
attached := app.Attach(srv, appkit.AttachOptions{
	Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
		return appkit.Identity{User: ti.UserID}, nil
	},
})
mux.Handle("/mcp", requireAuth(attached.Wrap(h)))
```

## v0.1.0

The first release.
