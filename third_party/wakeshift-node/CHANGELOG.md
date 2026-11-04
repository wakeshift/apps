# Changelog

What changed in each release of `@wakeshift/sdk`. 0.4.0 is the first published release: to start with it, read the [README](README.md). Its notes below, and the earlier releases', are for apps that ran the preview builds.

## 0.10.2 (2026-11-04)

### Added

- **Complete MCP tool annotations on Wakeshift's own tools.** `subscribe_events`, `unsubscribe_events`, `list_subscriptions` and `wakeshift_wait` now list a title and all four hints (`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`) in tools/list, so app stores that require them, such as ChatGPT's, accept your server. `subscribe_events` and `unsubscribe_events`: idempotent, and not read-only, destructive or open-world. `list_subscriptions`: read-only and idempotent (as before), and now not destructive or open-world. `wakeshift_wait`: not read-only, destructive, idempotent or open-world. Before, only `list_subscriptions` had annotations, so clients read the others with the spec's defaults: destructive and open-world. Every SDK gives the same ones, on an McpServer, with `attachMastra` and with `attachXmcp` (which serves no wait tool).

## 0.10.1 (2026-11-03)

### Fixed

- **A relayed beacon line never ends a wrapped tool's result.** If your tool's own text ended in a line that looked like a Wakeshift beacon (for example, a chat message someone posted, relayed in your result), the SDK kept that line last and dropped Wakeshift's own. An agent's hook acts on the last line, so someone could get a beacon minted for your tool, post it, and connect another person's conversation to their own session when that person's agent called the tool. The SDK now strips beacon lines from the end of your text and always puts Wakeshift's beacon last. Upgrade if any wrapped tool returns content other people wrote.
- **tools/list no longer stalls on the app-name lookup.** A slow or failing `GET /app` made every tools/list wait up to 20 seconds, and the failure wasn't remembered. The lookup now waits at most 3 seconds, waits 60 seconds before asking again after a failure, and keeps a name it was given.
- **A result that asks the client for input isn't final.** On protocol 2026-07-28 a tool can answer with an input request (an elicitation) and the client calls again. The SDK kept what that round subscribed and added Wakeshift's block next to the input request, which MCP SDKs refuse; it now rolls the round back and adds nothing.

## 0.10.0 (2026-11-03)

### Changed

- **`wakeshift_wait` is opt-in.** `attach` (and `attachMastra`) no longer serves Claude Code's wait tool unless you turn it on: `wake.attach(server, { tools: { wait: { enabled: true } } })`. While a conversation waits on it, Claude Code keeps a request open to your server for about 20 seconds at a time and sends the next about a second later, for as long as it waits, whether or not anything arrives: on a serverless host, a function invocation per waiting conversation, all the time. This changes behavior for apps on today's defaults: a recent Claude Code in the terminal whose user declines Wakeshift's plugin now waits with a background monitor, as every other Claude Code does, rather than on your server. In manual permission mode that means approving the monitor, and each restart every 25 minutes, instead of one approval of `wakeshift_wait`; and the wait no longer carries on through `claude --continue`. Users who install the plugin see no difference. To keep the wait tool, add `tools: { wait: { enabled: true } }`, on a stateless server, and on a serverless host give the function a timeout of at least 30 seconds. Without it, `server/discover` no longer offers the tasks extension, tools/list no longer lists `wakeshift_wait`, and Wakeshift's `tasks/get` and `tasks/cancel` go on to your MCP SDK.
- **`tools` takes an object, each tool `{ enabled }`**: `subscribe`, `list` and `unsubscribe` (on unless disabled, as before) and `wait` (off unless enabled). `tools: { subscribe: { enabled: false } }` replaces `tools: ["unsubscribe_events", "list_subscriptions"]`. New types: `AttachTools`, `ToolOptions`, `WaitToolOptions`.

### Deprecated

- **`tools` as a list of names** (`BUILTIN_TOOLS`). It still works in this release, adding the tools it names and never the wait tool, and warns once through `onWarning`.

## 0.9.0 (2026-11-03)

### Added

- **`subscribes`: a tool declares the events it subscribes to.** `attached.events("watch_build", { subscribes: [buildFinished] }, handler)`. tools/list then adds a sentence to the tool's description, written from each event's own description and naming your app as the console has it: "This tool is how Shipyard notifies you in this conversation of this event: A build finished: it passed, failed or was canceled. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." Several events read "of these events: …; …". Every tool that subscribes gets the sentence without writing it: agents set Wakeshift's part of a result aside as something the user didn't ask for unless the tool's description says the tool notifies through it. `call.subscribe` takes only the declared events (a type error otherwise), and a tool that declares `subscribes: []` has no `call.subscribe`. Mastra's adapter takes the same option; xmcp's does too, with `attached.describe(description, { subscribes })` for the tool file's metadata, since xmcp writes tools/list itself. The Go SDK has `appkit.Subscribes`, the Python SDK `@attached.tool(subscribes=[…])`. Write event descriptions as plain statements of what happened, since agents now read them in your tools' descriptions.
- **`IdReusedError`: an emit's `id` used for a different emit is refused.** Wakeshift deduplicated emits on `id` alone, so reusing an `id` by accident (a timestamp or a commit SHA two occurrences share) for another event, other arguments, recipients or payload dropped the second emit silently, and it returned the first's `matched`, reading as delivered. Now Wakeshift compares what each emit sends: a retry of the same emit with its `id` still sends nothing new and returns the first's result with `duplicate: true`, and the same `id` on a different emit is refused with `409 id_reused` and sends nothing. `emit` throws `IdReusedError` for it, a `WakeshiftError` with code `id_reused` (`isCode(err, "id_reused")` works too). Recipients compare as a set, so a retry may list them in another order, and `"u1"` and `["u1"]` are the same.

### Deprecated

- **A subscribe the tool doesn't declare.** `call.subscribe` from a tool wrapped without `subscribes` (`attached.events(name, handler)`), or to an event its `subscribes` doesn't list, still works in this release, and warns once per tool through `onWarning`, naming the tool and the fix. From the next release it throws. The two-argument `attached.events(name, handler)` stays, for tools that don't subscribe.

### Changed

- **`notifies` is an override.** Tools that declare `subscribes` get the sentence without it. Use `notifies(description, { when })` only for your own wording: a description that already has its sentence gets no second one.

### Fixed

- **An emit to one user with nothing subscribed lists them in `unmatched`.** It returned `matched: 0` and an empty `unmatched`, so the recipient was in neither; an emit to several listed them correctly. Every recipient is now in `matched`'s count, `unmatched` or `ended`, whether `to` names one or many.
- **`matched` is described the same everywhere**: the conversations an emit reached, each woken now or as soon as its agent can take it (the README said "woken").

## 0.8.0 (2026-11-03)

### Added

- **`notifies(description)`, for tools that subscribe.** It returns your tool's description with a few sentences after it, the same in every SDK. For an app named Shipyard, with `when` "a build finishes": "This tool is how Shipyard notifies you in this conversation when a build finishes. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." The app is named as the console has it (tools/list asks Wakeshift for the name once); `app` names it yourself, and with no name it says "this app". `when` is optional. Use it on the tools that call `call.subscribe` (`description: notifies("Watch a build until it finishes.", { when: "a build finishes" })`). In wakebench's realistic tier (a CI app, a user who never mentions Wakeshift), Claude Code set Wakeshift's part of a subscribing tool's result aside as something the user didn't ask for unless the tool's own description said the tool notifies through it. The Go SDK has the same as `appkit.Notifies` (`appkit.NotifyApp`, `appkit.NotifyWhen`).

### Fixed

- **Emitting before any agent connects works.** An emit at startup, or from a webhook, before any agent had connected to your MCP server failed with `404 unknown_event`: the manifest was sent only once `attach` had run, and with `createMcpHandler` that's the first request. Now, when Wakeshift answers an emit with `unknown_event`, the SDK sends your manifest (a merge, `POST /manifest/merge`, so it never removes an event another process sent, and carries only the settings you passed to `publishManifest`) and emits once more. Emits that fail together send it once, and an emit Wakeshift already knows the event for costs nothing extra. A process that only emits now sends its events this way too, when Wakeshift doesn't have them. Calling `publishManifest()` at startup still works, but you no longer need it. One consequence: a running process that still defines an event you removed with `publishManifest` adds it back on its next emit, so stop defining it first.
- **`unknown_event` says why.** Wakeshift's message now names the event and the likely cause (no manifest sent yet), instead of "no event by that name". The code is unchanged.

## 0.7.3 (2026-11-02)

### Fixed

- **An unsubscribe without the session key says so.** When a wrapped tool's `call.unsubscribe` ends nothing because the call didn't pass the conversation's `session_key` (Claude Code's calls name no conversation, so the key is the only way Wakeshift finds it), Wakeshift's block on that call now tells the agent that nothing ended, to call again with the key it was given when it subscribed, and not to subscribe again until it has. `call.unsubscribe` still returns `false`, and the note follows your tool's own text. Before, the agent saw only your text ("This chat wasn't watching…"), subscribed again without the key, and got every message twice. The built-in `unsubscribe_events` tool's reply carries the same note.

## 0.7.2 (2026-11-02)

### Fixed

- **`@modelcontextprotocol/server` 2.3.0 or later.** The peer dependency was `^2.0.0`, but on 2.0, 2.1 and 2.2 Claude Code's `wakeshift_wait` never completes: those releases answer `tasks/get` and `tasks/cancel` on protocol 2026-07-28 with "Method not found" themselves, before `attach` sees them ([modelcontextprotocol/typescript-sdk#2598](https://github.com/modelcontextprotocol/typescript-sdk/issues/2598), fixed in 2.3.0 by [#2599](https://github.com/modelcontextprotocol/typescript-sdk/pull/2599)). So conversations told to use the wait tool weren't woken. Everything else (wrapped tools, the built-in tools, MCP Events with `events/poll` and webhooks) works on 2.0 and later. The peer range is now `^2.3.0`: upgrade with `npm install @modelcontextprotocol/server@^2.3.0`. `@modelcontextprotocol/sdk` 1.x is unaffected.
- **`attach` warns on an older 2.x.** Given a server from `@modelcontextprotocol/server` before 2.3.0, `attach` warns once (through `onWarning`), naming the version to upgrade to.

## 0.7.1 (2026-11-01)

### Fixed

- **Subscribing again after unsubscribing keeps the conversation's session.** The reply from the built-in `unsubscribe_events` tool now tells the agent its session key stays the same and to pass it when it subscribes again. Without it, Claude Code often subscribed again without the key, which started a second session and, on the MCP task path, a second `wakeshift_wait`.

### Changed

- The README and docs lead with `@modelcontextprotocol/server` 2.x; 1.x is the alternative. Nothing changes in the code.

## 0.7.0 (2026-10-31)

### Added

- **Mastra.** `@wakeshift/sdk/mastra` serves Wakeshift from a Mastra `MCPServer` (`@mastra/mcp` 2.7 or later): `attachMastra(wake, opts)`, then `attached.inputs` and `attached.events` for the Mastra tools that subscribe, and `attached.serve(new MCPServer({ … }))`. Everything `attach` gives an `McpServer` works there: Wakeshift's block (text and `_meta`) in your wrapped tools' results, the built-in tools, MCP Events, Claude Code's `wakeshift_wait`, and who's calling (`identify` gets the request's `ctx`). Mastra's own MCP Events keep working beside Wakeshift's: `events/list` lists both. See [Mastra](README.md#mastra).
- **xmcp.** `@wakeshift/sdk/xmcp` brings Wakeshift into an xmcp app through its tool files: `attachXmcp(wake, opts)`, then `attached.inputs` and `attached.events` in the tools that subscribe, and `attached.tool("subscribe_events")` (and the other two) for a tool file each. xmcp offers no way to add MCP methods, so clients that subscribe natively (ChatGPT) and Claude Code's `wakeshift_wait` aren't served there; Claude Code and every other agent are woken their usual ways. See [xmcp](README.md#xmcp).
- Both are subpath exports, so an app that uses neither loads neither, and `@mastra/mcp` and `xmcp` are optional peer dependencies.

### Changed

- **A smaller package.** It no longer ships source maps (`dist/*.js.map`), which carried the SDK's TypeScript source inline: about half the unpacked size. Stack traces point at the compiled JavaScript in `dist/`, which is readable as it is.
- **A clearer error from `attach`.** Given something other than an `McpServer`, `attach` says what it got and what it takes, and, for a framework it recognizes (Mastra's `MCPServer`, punkpeye's `fastmcp`, a low-level `Server`, a Cloudflare `McpAgent`), what to do instead (for Mastra, `attachMastra`), with a link to [the frameworks Wakeshift works with](https://wakeshift.ai/docs/frameworks). It says to call `attach` before connecting only when the server is already connected.

## 0.6.0 (2026-10-31)

### Breaking

- Both MCP SDKs are optional peer dependencies: install the one you use. npm no longer installs `@modelcontextprotocol/sdk` for you, so list it in your app's own dependencies if your code imports it (as the install line has always said).

### Added

- **`@modelcontextprotocol/server` 2.x.** `attach` now takes an `McpServer` from the MCP TypeScript SDK 2.x (2.0 or later) as well as from `@modelcontextprotocol/sdk` 1.x, and tells them apart by the server you pass. Everything works the same on both: wrapped tools (Wakeshift's block in text and in `structuredContent`, rollback, `call.subscribe`), the built-in tools, MCP Events (`events/list`, `events/subscribe`, `events/unsubscribe`, `events/poll`), Claude Code's `wakeshift_wait` with `tasks/get` and `tasks/cancel`, and who's calling. 2.x speaks protocol 2026-07-28 itself, so with `createMcpHandler` clients on either revision connect with nothing more to set up. See the [README](README.md).
- On 2.x, `identify`, `events.native` and a wrapped tool's `call.extra` get 2.x's request context (`ctx`: the token in `ctx.http?.authInfo`), and are typed as it. On 1.x they get `extra`, as before.
- `attached.inputs` and `attached.output` take a zod object as well as a shape (`attached.inputs(z.object({ table: z.string() }))`), as 2.x's `registerTool` does. Shapes work as before.

### Changed

- The SDK's published types no longer import either MCP SDK, so they load with only one installed. `attached.events` returns a callback typed from your tool's arguments and your MCP SDK's context; `ToolCallExtra` is now the parts of a request's context Wakeshift reads, whichever SDK serves it (a wrapped tool's `call.extra` is still your MCP SDK's own type). If you passed a `ToolCallExtra` where 1.x's `RequestHandlerExtra` is expected, use `call.extra` instead.
- A JSON-RPC error from `attach`'s own methods (for example `events/subscribe` for an event your app doesn't offer) carries just its message, without the MCP SDK 1.x's `MCP error <code>: ` prefix in front of it.

## 0.5.2 (2026-10-30)

### Added

- **Claude Code is woken with nothing to install, and no restarts.** If your server's transport is stateless, `attach` now serves `wakeshift_wait` to clients that take MCP tasks: a recent Claude Code in the terminal, where Anthropic has turned them on. Wakeshift tells those conversations to call it after they subscribe; it returns at once and waits in the background, through `/compact` and restarts of Claude Code, and wakes the conversation when your events arrive. Other clients don't see it, and keep the instructions they had. Nothing to change in your code.

### Fixed

- **Agents that see only `structuredContent` are told how they're woken.** Some hosts, Claude Code among them, show the model a tool's `structuredContent` and not its text. A wrapped tool that returned `structuredContent` without `attached.output` left Wakeshift's instructions out of it, so an agent could say it was watching and never be woken. Now every structured result of a wrapped tool carries them in its `wakeshift` field, and `tools/list` adds that optional field to the tool's output schema, so clients that check results against the schema accept it. `attached.output` still declares the field in your own schema; it's no longer required. An output schema that refuses fields it doesn't name (`z.strictObject`) gets the result without the field, and a warning. Nothing to change in your code.
- On protocol 2026-07-28, `server/discover` and the lists' results now say how long a client may cache them (`ttlMs`, `cacheScope`), as that revision requires.

## 0.5.1 (2026-10-30)

Never published to npm. Everything below first shipped in 0.5.2.

### Added

- **Clients that speak MCP Events can poll.** `attach` now answers `events/poll`, and `events/list` offers `poll` beside `webhook` for every event. A client that can't take webhooks (an agent with no public URL, or Mastra in its poll mode) subscribes with its first poll, then each poll returns what happened since its cursor, in order. A poll's cursor acknowledges everything before it, so an event isn't lost when a poll fails: the next poll returns it again. Your `authorize` runs on every poll. Wakeshift keeps the subscription while the client polls (it lapses after 10 minutes without a poll), lists it with the user's other subscriptions (`list_subscriptions` says "polled by an MCP client"), and answers the client's next poll with `Forbidden` once the user stops it. Nothing to change in your code.
- **Clients that speak only MCP protocol 2026-07-28 can connect.** MCP Events is defined for protocol 2026-07-28, which the MCP TypeScript SDK 1.x doesn't have, and OpenClaw speaks nothing older. If your server's transport is stateless (no sessions), `attach` now answers `server/discover` and adds `resultType` to every result of a 2026-07-28 request, so OpenClaw can subscribe to your events. Clients that also speak the older protocol (Mastra, TanStack AI, the Vercel AI SDK) use 2026-07-28 too once your server offers it. A server with sessions is unchanged: those clients keep using `initialize`.
- **Webhooks for an event you no longer offer end.** When you remove an event from your app's manifest (with `publishManifest`), Wakeshift ends each webhook subscription to it within an hour, telling the client the event is gone (`terminated`, with `NotFound` and `data.kind: "event"`), so the client lists your events again rather than waiting for its next refresh. A poll for it gets `NotFound` at once. Nothing to change in your code.

### Changed

- `attach` follows SEP-3415 as revised on 2026-10-27. `ResourceExhausted`'s data is now `{"limit": "subscriptions"}`, the draft's name for it (it was `{"kind": "subscriptions"}`). `events/stream` still answers `Unsupported`: push isn't offered.

## 0.5.0 (2026-10-29)

### Breaking

- **`recognised` is now `recognized`.** The `Recognised` type is `Recognized`, and `Subscription.recognised` and `ToolCall.recognised` are `recognized`. Rename them where you read them: `sub.recognised.harness` becomes `sub.recognized.harness`. What they hold is unchanged.

### Fixed

- **Processes that share your key no longer remove each other's events.** In 0.4.0 every process sent the events it defined as your app's whole list. With an MCP server and a separate emitter on one key, whichever sent last removed the other's events, and the other's emits failed with `unknown_event` until it restarted. Now only your MCP server, the process that calls `attach`, sends the manifest automatically, and Wakeshift merges it into the one it has: it adds or updates your events and the settings you passed to `publishManifest`, and never removes an event or clears a setting. A process that doesn't call `attach` never sends it, whatever it calls (`emit`, `status`). `publishManifest` still replaces the whole manifest, and is how you remove an event.
- A process with no events defined never sends the manifest. In 0.4.0 any app call from one (even a status read) sent an empty list, which removed every one of your app's events.
- If an event is defined only in a process that emits it, define it in your MCP server too (or publish it with `publishManifest`): until a server sends it, emitting it fails with `unknown_event`.

## 0.4.0 (2026-10-29)

Define your app's events (`defineEvent`), attach Wakeshift to your MCP server (`attach`), subscribe the conversations that call your own tools (`attached.events`, `call.subscribe`), and emit to them (`emit`). The rest of this section is what changed since 0.3.0.

### Breaking

One way to subscribe from your own tools. Wrap the tool with the handle `attach` returns, and subscribe with `call.subscribe`: Wakeshift adds what the agent needs to be woken to whatever you return, once per call however many subscriptions it makes, and ends the subscriptions of a call that fails. The tool can't leave a conversation unwakeable by a forgotten argument, a wrong tool name or a result it didn't pass through.

A join tool:

```ts
// before
server.registerTool("join", { inputSchema: { table: z.string(), ...wake.args } }, async ({ table, ...wakeArgs }, extra) => {
  const sub = await turn.subscribe(extra, { table }, { server, user: myUser(extra), wakeArgs, tool: "join" })
  return sub.result(`You're seated at ${table}.`)
})

// now
const attached = wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })
server.registerTool(
  "join",
  { inputSchema: attached.inputs({ table: z.string() }) },
  attached.events("join", async ({ table }, call) => {
    const sub = await call.subscribe(turn, { table })
    // Keep sub.user and sub.id to reach this conversation: emit's to: [{ user, subscription: id }]
    return `You're seated at ${table}.`
  }),
)
```

A tool with structured output: make its `outputSchema` with `attached.output(shape)`, and the wrapper sets its `wakeshift` field to what Wakeshift adds, for hosts that show the model only `structuredContent`. Wakeshift's text stays in `content` too.

```ts
// before: sub.result(...) added Wakeshift's text to content only
// now
server.registerTool(
  "look",
  { inputSchema: attached.inputs({ table: z.string() }), outputSchema: attached.output({ seats: z.number() }) },
  attached.events("look", async ({ table }, call) => {
    await call.subscribe(turn, { table })
    return { content: [{ type: "text", text: "3 seats." }], structuredContent: { seats: 3 } }
  }),
)
```

A leave tool:

```ts
// before
server.registerTool("leave", { inputSchema: { table: z.string(), ...wake.args } }, async ({ table, ...wakeArgs }, extra) => {
  await turn.unsubscribe(extra, { table }, { server, user: myUser(extra), wakeArgs, tool: "leave" })
  return { content: [{ type: "text", text: `You left ${table}.` }] }
})

// now
server.registerTool(
  "leave",
  { inputSchema: attached.inputs({ table: z.string() }) },
  attached.events("leave", async ({ table }, call) => {
    const left = await call.unsubscribe(turn, { table })
    return left ? `You left ${table}.` : `You weren't at ${table}.`
  }),
)
```

A tool you used to pass to `wake.annotate` is wrapped the same way, without a subscribe: the wrapper adds what Wakeshift has for the conversation calling it.

Topics are gone: use events. Define an event for what you published to a topic, subscribe from a wrapped tool (or let agents use `subscribe_events`), and emit it.

```ts
// before
await wake.publish(`ci/${repo}`, { text: `CI ${run.conclusion} on ${repo}`, data: run, id })
const { subscriptions, connected } = await wake.topicStatus(`ci/${repo}`)

// now
const ciFinished = wake.defineEvent("ci.finished", {
  description: "CI finished on a repo",
  args: z.object({ repo: z.string() }),
  payload: z.object({ conclusion: z.string() }),
  text: (run, { repo }) => `CI ${run.conclusion} on ${repo}`,
})
await ciFinished.emit({ repo }, run, { id })
const { subscriptions, connected } = await ciFinished.status({ repo })
```

`wake.send(id, …)` is an emit to one subscription of a personal event (`to: [{ user, subscription: id }]`), `wake.subscriptionStatus(id)` is `event.statusOf([{ user, subscription: id }])`, `wake.publishUrl(topic)` is `event.publishUrl(args)`, and `wake.endSubscription(id)` and `wake.unsubscribeUser(user, topic)` are `event.end(user, id)` for each subscription you keep.

Groups are gone: keep membership in your app and emit to your members' subscriptions. `AgentScope.groups` is removed; store the `{ user, id }` each member's `call.subscribe` returned and emit to them in one call, `to: [{ user, subscription: id }, …]`.

Errors the SDK throws before asking Wakeshift have the status and code Wakeshift gives for the same mistake, the same in the Go and Python SDKs:

- A payload that doesn't match the event's schema is `bad_arguments`, Wakeshift's code for an emit's payload, not `bad_payload`. `ArgsError`, for arguments or a payload that don't match, is a `WakeshiftError` with code `bad_arguments` (status 400): its `message` starts `wakeshift: bad_arguments (400): `, and `detail` is the message without it.
- `call.subscribe` or `call.unsubscribe` given another app's event is `unknown_event` (404), not `bad_arguments`.
- `statusOf` with more than 1,000 subscriptions, or one without its user, is `bad_request`, not `bad_arguments`.

**A manifest's `events` add to the ones you define.** `publishManifest`'s `events` used to replace the events `defineEvent` made. Now Wakeshift gets both, the listed ones first, and an event both defined and listed is an error: nothing is sent. List only events defined elsewhere.

**A `filter` may not name a field an optional argument filters.** Without a `topic`, `defineEvent` adds a filter for each optional argument to your `filter`; where both named the same payload field, the argument's entry silently replaced yours. Now `defineEvent` throws, naming the field: remove it from `filter`, or pass `topic` and `filter` both.

### Removed

- `event.subscribe(extra, …)` and `event.unsubscribe(extra, …)`: `call.subscribe` and `call.unsubscribe` in a wrapped tool.
- `CallerSubscription` and its `result` and `agent`: `Subscription`, with `recognised`; the wrapper adds the block.
- Topics and their types: `wake.subscribe`, `wake.unsubscribe`, `wake.publish`, `wake.send`, `wake.topicStatus`, `wake.subscriptionStatus`, `wake.publishUrl(topic)`, `wake.closeTopic`, `wake.endSubscription`, `wake.unsubscribeUser`; `TopicSubscription`, `TopicEvent`, `TopicStatus`, `SubscriptionStatus`, `SendStatus`, `SubscribeOptions` and `Published` (its `matched` and `duplicate` are on `Emitted`). `PublishUrl.topic` goes with them.
- `wake.args`: `attached.inputs(shape)`.
- `wake.call`, `wake.callOf`, `Call`, `CallOptions`, `ClientInfoSource`, `applyAddition`, `wake.addition`, `wake.annotate` and `Addition`: the wrapper.
- `UNKNOWN_CLIENT_WARNING`, which only the removed calls gave. A wrapped tool's `call.recognised` says what Wakeshift recognized the agent as.
- `AgentScope.groups`.
- `AppKey.mode` and `wake.mode`, which meant nothing since current keys came in. Older keys (`wst_test_…`, `wst_live_…`) still work and name their app; a `wst_test_` key still needs a core.
- `session` as a hidden alias of `session_key`: a wrapped tool takes `session_key` only, and a `session` argument is dropped like any other the tool doesn't take.
- Reading a client's top-level `events` capability, the earlier MCP Events sketch's key: a client speaks MCP Events if it declares the extension (`extensions["io.modelcontextprotocol/events"]`) or is ChatGPT. `attach` still advertises both keys, since ChatGPT reads the top-level one.
- `speaksEvents`: a wrapped tool's `call.native` says whether its caller subscribes to your events itself.
- `LEGACY_KEY`, the top-level capability key `attach` advertises for ChatGPT. It's no longer exported; `EXTENSION_KEY` is the one a client declares.

Kept: `defineEvent`, `emit`, `event.publishUrl`, `event.status` and `event.statusOf`, `event.end`, `attach` with its three tools and native MCP Events, `revokePublishUrl`, `forgetUser`, `publishManifest`, `workspace`, `addAgent`, `checkAgent`, `harnessAgent`, `SESSION_ARG`, `HARNESS_ARG`, `CONVERSATION_ARG`, keys and errors.

### Added

- `wake.attach` returns a handle, `Attached`. Wrap your own tool's callback with `attached.events("name", handler)` and make its input schema with `attached.inputs(shape)` (a type error without it): the handler gets the tool's own arguments and a `ToolCall` (`user`, `tool`, `native`, `nativeConversation`, `session`, `recognised`, `extra`), subscribes the calling conversation with `await call.subscribe(event, args, { ttl?, reason?, label? })` (a `Subscription`: `{ id, user, created, session, recognised, native }`) and ends it with `call.unsubscribe`. A `ToolCall` used after its handler returned throws `CallEnded`.
- `call.resolveSession()` finds or starts the conversation's session without subscribing it. `call.subscribe`'s `reason` is shown in the console.
- `identify` can return the MCP client's name, `{ user, client }`, for a stateless server that never sees `initialize`: Wakeshift reads it to tell which program the agent runs in, and whether the client subscribes to events itself. A name the request carries (`initialize`'s, or its `_meta`'s) wins. You no longer need to patch `server.server.getClientVersion` for it.
- `attach`'s `tools` option names which of its own tools it adds (`BUILTIN_TOOLS`: `subscribe_events`, `unsubscribe_events`, `list_subscriptions`). All three by default; `[]` adds none.
- `attached.output(shape)` adds an optional `wakeshift` string to an output schema, which the wrapper fills with the block's text, for hosts that show only `structuredContent`.
- A personal event's `emit` takes many recipients: `to` is a user, a `Recipient` (`{ user, subscription }`, exactly that subscription; `{ native: user }`, only that user's own MCP Events subscriptions) or a list of them, mixed, up to 1,000. Its result, `Emitted`, adds `unmatched` and `ended`. `publishUrl` still takes exactly one user.
- `event.status(args, { to? })`: how many conversations are subscribed to an event with these arguments (a personal event: one user's), how many are connected now, and how. `event.statusOf([{ user, subscription }, …])` reads up to 1,000 subscriptions by ID: whether each is live and its conversation connected.
- `AppInfo.key`: the key's console ID and name, which `wake.app()` returns, as the Go and Python SDKs do.
- `Manifest.preapprove_tools`: your tools, by name, that agents' programs can run without asking each time (at most 20). When a user connects Claude Code or Cursor CLI, Wakeshift offers to allow exactly these in the same question as its install.
- `Manifest.others_can_wake`: your app wakes its users' agents with what other people and their agents do there, so the consent screen says agents of people they don't know can wake theirs.

### Changed

- A wrapped tool's reply carries one Wakeshift block, however many subscriptions the call makes, and only what the conversation needs.
- A subscription a failed tool call made is ended: a call that throws, returns `isError`, returns output its `outputSchema` refuses, or is canceled.
- A client that speaks MCP Events is told which event to subscribe to, or that it has: `call.native` is true, and `call.subscribe` makes nothing. The SDK recognizes ChatGPT's `_meta["openai/session"]`, its client name or User-Agent, and clients that declare MCP Events, on stateless servers too; `attach`'s `events.native(extra)` names callers it can't recognize, such as your own agents.
- `session_key` is the one name for the session argument.
- `attach`'s own tools (`subscribe_events`, `unsubscribe_events`, `list_subscriptions`) describe `session_key` to the agent as a wrapped tool does.
- `list_subscriptions` is marked read-only and idempotent (`readOnlyHint: true`, `idempotentHint: true`), so clients that run read-only tools without asking (Codex, by default) don't ask before it.
- You needn't call `publishManifest`. Wakeshift gets the manifest automatically the first time your app needs it (an emit, a status, a publish URL, a subscription), and again after a `defineEvent`. The automatic send carries everything your app configured: the manifest you last passed to `publishManifest` (even if that call failed), or else the description, agent scope, `others_can_wake` and `preapprove_tools` Wakeshift already has. Before, an automatic send before your first `publishManifest` carried only the events, wiping the rest.
- An emit's `to` that's an empty string, or only whitespace, is refused (`bad_arguments`: a recipient's user is empty), as an empty user in a list is and as the Go and Python SDKs do. It was taken as no recipient.

### Fixed

- `statusOf` trims each subscription ID, as it does each user, and refuses one that's only whitespace (`bad_request`): every SDK trims the whitespace around recipients' names.
- A successful answer that is a JSON array is a `bad_response` error, as it is in the Go and Python SDKs.
- An error whose `code` isn't a string has an empty code, and one whose `message` is empty takes the body's start, else the status's text.
- An MCP Events client gets "Wakeshift couldn't be reached" when the core is unreachable, without the core's address; a `callback_failed` error's `data.reason` is always a string.
- Google's 500 page is recognized by "Server Error" within the body's first 4 KB.
- A failed tool call's rollback no longer ends a subscription that another call on the same conversation renewed in the meantime (two calls in parallel, or a second call while the first was still running). The call that renewed it told the agent it would be woken; now it is.
- `defineEvent` no longer adds the default route's filter to the `filter` object you passed: it copies it.
- A personal event's `emit` without `to` says to pass the subscribers it's for, not one.

## 0.3.0 (2026-10-26)

### Breaking

Guest sign-in is gone. Your app identifies its own users with its own sign-in, and passes `identify` to `attach` to say who is behind each request. Apps on 0.2 that use `GuestAuth` must replace it with their own sign-in before upgrading.

Removed:

- `GuestAuth`, `guestOf` and their types `Guest`, `GuestAuthOptions` and `GuestAuthInfo`.
- `Wakeshift.secretSeed()`, which only `GuestAuth` used.
- `attach`'s default `identify`, which read the guest `GuestAuth` signed in. Without `identify`, requests have no user, so personal events and `list_subscriptions` refuse.

```ts
// before
wake.attach(server)
// now
wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })
```

`wake.workspace`, `wake.addAgent`, `wake.checkAgent` and `harnessAgent` stay: they serve the users your own sign-in identifies.

### Added

- `Call.tool`, set with `wake.call(extra, { tool })` or `wake.callOf({ tool })`: the name of the tool being called. The connector beacon Wakeshift adds to a result names that tool, and Wakeshift's hooks act on it only from that tool's result, so pass it for wake-ups to connect with updated hooks. The tools `attach` adds, and `defineEvent`'s `subscribe` and `unsubscribe` (new `tool` option), pass theirs.

### Changed

- An app's user has at most 20 live MCP Events webhook subscriptions. Wakeshift refuses one more with `403 subscription_limit`, which `attach` answers with MCP Events' `ResourceExhausted` (`InvalidParams` for ChatGPT), with data `{ kind: "subscriptions" }`.

## 0.2.0 (2026-10-26)

### Breaking

Sign in with Wakeshift is gone: Wakeshift no longer gives apps grants in people's own workspaces. Your app identifies its own users, with `GuestAuth` or its own sign-in, and acts for the agents in the workspaces it keeps (`wake.workspace`, `wake.addAgent`, `wake.checkAgent`). Topics, events and `GuestAuth` work as before.

Removed:

- `WakeshiftSignIn`, `SignInError` and their types `WakeshiftSignInOptions` and `StartOptions`.
- `Agent` and `Claims`.
- On `Wakeshift`: `protectedResource()`, `challenge()`, `verifyAccessToken()`, `exchange()`, `pkce()`, `authorizeUrl()`, `signIn()`, `refresh()` and the `mcpUrl` option and property.
- The types `SignedIn`, `AuthorizeOptions` and `Connection`.
- `Manifest.accounts`, `Manifest.workspace` and `AgentScope.redirect_uris`. Wakeshift refuses a manifest with `accounts: "wakeshift"`, a workspace scope or redirect URIs (`bad_manifest`).
- `serveEvents`' `agent` option, the `agent` option of `wake.call`, `event.subscribe` and `event.unsubscribe`, and `Call.agent_token`.

`serveEvents` is renamed `attach`, the one call that attaches Wakeshift to your MCP server. Its `user` option is replaced by `identify`, which returns an `Identity` (`{ user }`); the type `ServeEventsOptions` is now `AttachOptions`. It behaves as before:

```ts
// before
wake.serveEvents(server, { user: (extra) => myUser(extra) })
// now
wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })
```
