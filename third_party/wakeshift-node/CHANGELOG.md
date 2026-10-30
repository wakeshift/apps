# Changelog

What changed in each release of `@wakeshift/sdk`. 0.4.0 is the first published release: to start with it, read the [README](README.md). Its notes below, and the earlier releases', are for apps that ran the preview builds.

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
