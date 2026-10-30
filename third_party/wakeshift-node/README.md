# @wakeshift/sdk

Let agents subscribe to things in your app. You define the events your app offers, serve them on your MCP server, and emit them when they happen; Wakeshift wakes every subscribed agent, whether it's Claude Code, Codex, ChatGPT or another. Your events follow MCP Events: clients that speak it subscribe natively, and Wakeshift bridges every other agent. You don't deal with harnesses.

Node 22 or later. The package is ESM-only: load it with `import`, from an ES module (`"type": "module"`, or a `.mjs` or `.mts` file). It has no CommonJS build, so `require("@wakeshift/sdk")` works only where Node can require ES modules. Peer dependencies: `@modelcontextprotocol/sdk` (1.23 or later: the first that takes zod 4) and `zod` (3.25 or later, or 4). Import `z` from `"zod"`, not `"zod/v4"`: the SDK's schemas are built from `"zod"`, and the MCP SDK refuses a schema that mixes the two.

## Install

```sh
npm install @wakeshift/sdk @modelcontextprotocol/sdk zod
```

Install the package, not a checkout of this repository (a `file:` dependency): a `file:` dependency links the directory along with its `node_modules`, so the SDK can load its own development copy of zod instead of yours, and the MCP SDK then fails with "Mixed Zod versions detected in object shape". To try a change of your own, `npm pack` and install the tarball it writes.

## Quickstart: a CI bot

You have an MCP server and a GitHub webhook handler. Register an app in the Wakeshift console (Build), and put its key in `WAKESHIFT_KEY` (`wst_` and 50 characters). The SDK talks to `https://app.wakeshift.ai` unless `WAKESHIFT_CORE` names another address. Load them however you load environment variables, for example `node --env-file=.env server.ts` (Node 22.18 or later runs a `.ts` file directly).

```ts
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY! })

const ciFinished = wake.defineEvent("ci.finished", {
  description: "CI finished on a GitHub repo",
  args: z.object({ owner: z.string(), repo: z.string(), branch: z.string().optional() }),
  payload: z.object({ branch: z.string(), conclusion: z.string(), name: z.string(), url: z.string() }),
  authorize: (subscriber, { owner, repo }) => canSeeRepo(subscriber.user, `${owner}/${repo}`),
  text: (run, { owner, repo }) => `CI ${run.conclusion} on ${owner}/${repo} ${run.branch}: ${run.name}`,
})

// One McpServer per MCP session; serve the events before connecting it.
const server = new McpServer({ name: "ci-bot", version: "1.0.0" })
// myUser: your own sign-in's ID for the person behind the request.
wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })
```

```ts
// In your GitHub webhook handler, after checking X-Hub-Signature-256:
if (req.headers["x-github-event"] === "workflow_run" && body.action === "completed") {
  const run = body.workflow_run
  const [owner, repo] = body.repository.full_name.toLowerCase().split("/")
  await ciFinished.emit(
    { owner, repo },
    { branch: run.head_branch, conclusion: run.conclusion, name: run.name, url: run.html_url },
    { id: String(req.headers["x-github-delivery"]) },
  )
}
```

That's all. An agent subscribes with the `subscribe_events` tool `attach` adds (ChatGPT subscribes natively), and is woken with the text and payload of each occurrence for its repo, framed as information from your app, not as instructions from its user. A subscriber that names a `branch` only gets that branch's occurrences.

- `owner` and `repo` are required, so they route occurrences; their values are lowercase IDs (`a-z 0-9 . _ -`). The optional `branch` filters on the payload's `branch`.
- `attach` adds `subscribe_events`, `unsubscribe_events` and `list_subscriptions`, and touches no tool of yours you don't wrap (below).
- Native subscriptions (ChatGPT) and personal events need a signed-in user: your app signs its users in and passes `identify` to `attach`.

## Events

Events follow MCP Events (SEP-3415): you define the event types your app offers, serve them, and emit occurrences. Clients that speak MCP Events, such as ChatGPT, subscribe natively; every other agent subscribes with the `subscribe_events` tool `attach` adds, and Wakeshift wakes it its own way. You write no subscribe tool and handle no harnesses.

```ts
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY! })

const message = wake.defineEvent("chat.message", {
  description: "A new message in a channel",
  args: z.object({ channel: z.string() }),
  payload: z.object({ from: z.string(), text: z.string() }),
  text: (m) => `${m.from}: ${m.text}`,
})

// A personal event: each subscription belongs to its subscriber, and an
// occurrence goes to one of them, so it can carry their own private view.
const turn = wake.defineEvent("game.turn", {
  description: "It's your move at a table",
  args: z.object({ table: z.string() }),
  payload: z.object({ hole: z.array(z.string()), toCall: z.number() }),
  text: (t) => `Your move: ${t.toCall} to call`,
  personal: true,
})

// One server per connection; attach once, before connecting it.
const server = new McpServer({ name: "chat", version: "1.0.0" })
// myUser: your own sign-in's ID for the person behind the request.
const attached = wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })

// When something happens:
await message.emit({ channel: "eng" }, { from: "ada", text: "shipped it" })
await turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "user-123" })
```

- **`wake.defineEvent(name, { description, args, payload, text, authorize?, personal?, deleteOnDelivery? })`** returns the event. `args` is what a subscriber picks; its required string, integer and boolean fields route occurrences, so their values are lowercase IDs (a-z 0-9 . _ -), and an optional one filters on the payload field of the same name. `authorize(subscriber, args)` decides who may subscribe. With `deleteOnDelivery: true`, Wakeshift deletes each occurrence's text and payload once it's delivered. Define every event before `attach`, which throws with none.
- **`event.emit(args, payload, { id?, to? })`** checks the payload against its schema and sends it to everyone subscribed with those arguments. A personal event goes to its recipients, `to`: users (the `user` attach's `identify` gives for them), `{ user, subscription }` (exactly one conversation's subscription, the `id` `call.subscribe` returned) and `{ native: user }` (only that user's own MCP Events subscriptions), mixable, up to 1,000. It returns `{ matched, duplicate, unmatched, ended }`: `matched` counts the conversations woken, `unmatched` the users with nothing subscribed, `ended` the subscriptions no longer live (forget them). `id` makes retries emit once; a retry must name the same recipients.
- **`wake.attach(server, { identify?, tools?, events? })`** attaches Wakeshift to your MCP server and returns the handle your own tools use (below). It serves `events/list`, `events/subscribe` and `events/unsubscribe` to clients that speak MCP Events, and adds `subscribe_events` (for everyone else), `unsubscribe_events` and `list_subscriptions` (for every client, so a person can always see and stop what they're subscribed to); `tools` names fewer of them (`BUILTIN_TOOLS`; `[]` for none). `identify(extra)` says who is behind a request, as `{ user, client? }`: `user` is your app's ID for the person, from your own sign-in; without it, requests have no user, so personal events and `list_subscriptions` refuse. On a stateless server, which never sees `initialize`, `client` is the MCP client's name as your sign-in knows it (an OAuth registration's `client_name`, say): Wakeshift reads it to tell which program the agent runs in. A name the request carries wins. It also sends your app's manifest, with your events, the first time they're needed: it adds them and never removes one. A separate process that only emits never sends it.
- **`event.end(user, id)`** ends one of a user's subscriptions by its ID, outside a tool (a player who timed out, a member removed).
- **`event.status(args, { to? })`** counts the conversations subscribed with those arguments (a personal event: one user's), how many are connected now, and how: `{ subscriptions, connected, via: { stream, poll, hooks, webhook } }`. **`event.statusOf([{ user, subscription }, …])`** reads up to 1,000 subscriptions by ID: `[{ id, user, live, connected, via? }]`. Both only read.
- **`event.publishUrl(args, { to? })`** returns `{ id, event, url }`, a capability URL that emits the event with those arguments (a personal event: to one user) for services that only send webhooks. A POST's JSON body is the payload, checked against the event's schema. `wake.revokePublishUrl(id)` stops it.

### Subscribing from your own tools

A tool like "join" subscribes the conversation calling it: wrap its callback with the handle `attach` returns, and call `call.subscribe`. Wakeshift adds what the agent needs to be woken to whatever the tool returns, once per call however many subscriptions it makes, and ends the subscriptions of a call that fails.

```ts
// attached: what wake.attach(server, …) returned, above.
server.registerTool(
  "join",
  { description: "Take a seat at a table", inputSchema: attached.inputs({ table: z.string() }) },
  attached.events("join", async ({ table }, call) => {
    const sub = await call.subscribe(turn, { table }, { reason: `Playing at ${table}` })
    // Keep sub.user and sub.id to reach this conversation later:
    // turn.emit({ table }, hand, { to: [{ user: sub.user, subscription: sub.id }] })
    return `You're seated at ${table}.`
  }),
)
```

- **`attached.inputs(shape)`** is `shape` plus Wakeshift's three optional arguments, `session_key`, `harness` and `conversation_id`. A tool wrapped with `attached.events` needs them: without them it doesn't compile. An argument a tool doesn't declare is ignored, not refused.
- **`attached.events(name, handler)`** wraps the callback of the tool registered as `name` (the MCP SDK doesn't tell a callback its tool's name; the first call checks it). The handler gets the tool's own arguments and a `call`, and returns a string or a `CallToolResult`.
- **`call`**: `user` (from `identify`), `tool`, `native` and `nativeConversation`, `session` (the `session_key` passed; after a subscribe, the conversation's session), `recognized` (which program Wakeshift recognized the agent as, `null` before a subscribe) and `extra` (MCP's own). It works until the handler returns.
- **`await call.subscribe(event, args, { ttl?, reason?, label? })`** subscribes the calling conversation, or renews its subscription, and resolves to `{ id, user, created, session, recognized, native }`. `reason` is shown in your console. Your tool decides who may (`authorize` isn't consulted). **`await call.unsubscribe(event, args)`** ends it; **`await call.resolveSession()`** finds or starts the conversation's session without subscribing it.
- **A failed call**: one that throws, returns `isError`, returns output its `outputSchema` refuses, or is canceled, ends every subscription it made, and its reply gets nothing from Wakeshift.
- **Clients that subscribe themselves** (MCP Events: ChatGPT and others) aren't subscribed: `call.native` is true, and the reply tells them which event to subscribe to. `attach`'s `events.native(extra)` tells the SDK about callers it can't recognize, such as your own agents.
- **Structured output**: some hosts show the model only `structuredContent`. Make the tool's `outputSchema` with `attached.output(shape)`, and its `wakeshift` field carries what Wakeshift adds, too.

```ts
server.registerTool(
  "look",
  {
    description: "Look at a table",
    inputSchema: attached.inputs({ table: z.string() }),
    outputSchema: attached.output({ seats: z.number() }),
  },
  attached.events("look", async ({ table }) => ({
    content: [{ type: "text", text: `3 seats at ${table}.` }],
    structuredContent: { seats: 3 },
  })),
)

server.registerTool(
  "leave",
  { description: "Leave a table", inputSchema: attached.inputs({ table: z.string() }) },
  attached.events("leave", async ({ table }, call) => {
    const left = await call.unsubscribe(turn, { table })
    return left ? `You left ${table}.` : `You weren't at ${table}.`
  }),
)
```

A wrapped tool that subscribes to nothing, like `look`, still gets what Wakeshift has for the conversation calling it: occurrences held for an agent Wakeshift can't wake, or what it needs to reconnect. Usually that's nothing.

## Reference

### `new Wakeshift({ key, core?, fetch?, timeoutMs?, onWarning? })`

- `key`: `wst_` and 50 characters. It's your app's Ed25519 private key; it never goes over the wire. Each request carries a fresh assertion signed with it (valid two minutes). Older keys, `wst_test_<app>_<seed>` and `wst_live_<app>_<seed>`, keep working.
- `core`: the address of the console the key came from. Default: `WAKESHIFT_CORE` from the environment, else `https://app.wakeshift.ai`. An older `wst_test_` key has no default: it needs one of the two, and the constructor throws without them.
- `onWarning(message)`: called for problems that don't fail a call: each time for a failure, such as a failed call's subscription that couldn't be rolled back, and once for each kind of misuse, such as a wrapped tool that returned with a subscribe still in flight. Default: `console.warn`.
- `await wake.app()`: `{ id, name }`, which app the key is, as Wakeshift says. It asks once and remembers the answer.
- `wake.appId`: the app's ID. A current key doesn't name its app, so this throws until `await wake.app()` has answered; an older key names it. `wake.core`: the address in use.

### Users

- `wake.forgetUser(user)`: `false` if there was nothing to forget. Deletes everything Wakeshift keeps for the user: their conversations, subscriptions, events and deliveries, stream links (open streams close), publish URLs for them, and their ID. Your app's log records that a user was forgotten, without their ID or anything derived from it.

### Manifest and workspaces

- `wake.publishManifest({ description?, agent?, events?, others_can_wake?, preapprove_tools? })`: what your app may do; returns it as Wakeshift keeps it. `agent`: `{ wake, post, ttl_days, wake_limits?, start_prompt? }`, each scope event `{ pattern, description }`: what the agents in the workspaces your app keeps may be woken by and post. `others_can_wake`: other people and their agents wake your users' agents through your app (messages, moves), so the consent screen says so. `preapprove_tools`: your tools, by name, that agents' programs can run without asking each time (at most 20: tools that only read, and routine ones that reach only what the agent joined); when a user connects Claude Code or Cursor CLI, Wakeshift offers to allow exactly these. It replaces the manifest, so it's the way to remove an event. You needn't call it otherwise: your MCP server, the process that calls `attach`, sends the manifest automatically the first time it needs it (a subscription, a wrapped tool's call, an emit), adding your defined events to the ones Wakeshift has and never removing one or clearing a setting, so processes that share your key keep each other's events. A process that only emits never sends it. Once you've called it, every automatic send carries the settings you passed. `events` is only for events defined elsewhere: they go before your defined ones, and listing a defined one is an error.
- Your app identifies its own users with its own sign-in, and acts for the agents in the workspaces it keeps.
- `wake.workspace(user, name)`, `wake.addAgent(workspace, agent, displayName)`, `wake.checkAgent(workspace, agent)`: the workspace Wakeshift keeps for one of your users, an agent in it with a grant for your manifest's agent scope, and whether that grant is live. Wakeshift keeps a user's conversations in that workspace; you don't need these to subscribe them.
- `harnessAgent(clientName)`: the agent ID a client name maps to (`"Claude Code"` is `claude-code`), as Wakeshift names it.

### Errors

API errors throw `WakeshiftError` with `status` and `code`: the same codes as the [HTTP API](https://wakeshift.ai/docs/http-api)'s, and `unreachable`. The SDK checks some mistakes before sending (arguments and payloads against the event's schemas, recipients, another app's event) and throws the error Wakeshift gives for the same mistake, so the code is the same whichever caught it, and the same in the Go and Python SDKs:

| Status | Code | When |
| --- | --- | --- |
| `400` | `bad_request` | Something in the request is invalid, including a field the API doesn't take. The message says what. Also more than 1,000 subscriptions in one `statusOf`, or one without its user. |
| `400` | `bad_arguments` | An event's arguments or payload don't match its schemas, a routing value isn't a lowercase ID, or the recipients don't fit the event: a personal event's emit, publish URL or status without `to` (a publish URL's or status's names exactly one user), a shared event's with one, a recipient with an empty user or subscription, or `call.subscribe` on a personal event with no user. |
| `401` | `unauthenticated` | The assertion was refused: expired, reused, signed with another key, or for another `aud` (a key for another Wakeshift). The message says which. |
| `403` | `other_client` | The `session_key` is a conversation in another MCP client of the same user. Call again without it. |
| `404` | `unknown_event` | Your app offers no event by that name, or `call.subscribe` or `call.unsubscribe` was given another app's event (one defined on another `Wakeshift` client). |
| `404` | `unknown_publish_url` | The publish URL ID isn't one of your app's. |
| `413` | `too_large` | The request body is over 64 KB (512 KB for an emit or a status call, room for 1,000 recipients). The limit is on the whole body, so the payload must leave room for the rest of the request. |
| `429` | `rate_limited` | Your emits to lists named over 10,000 recipients in a minute. |
| `0` | `unreachable` | No answer at all: a wrong `core`, a network problem, or the timeout. The message names the core it tried. |

`isCode(err, "other_client")` checks one.

## Documentation

The guides and the full reference are at [wakeshift.ai/docs](https://wakeshift.ai/docs): the [quickstart](https://wakeshift.ai/docs/quickstart), [events](https://wakeshift.ai/docs/events), [your users](https://wakeshift.ai/docs/users) and [the TypeScript SDK](https://wakeshift.ai/docs/sdk). The same calls are in the [Go](https://github.com/wakeshift/wakeshift-go) and [Python](https://github.com/wakeshift/wakeshift-python) SDKs.

## Development

```sh
npm ci
npm run typecheck
npm test          # unit tests (node:test) and the README's examples
npm run build     # dist/
```

The end-to-end tests (`test/e2e*.test.ts`) run against a Wakeshift server, so they run in Wakeshift's own repository and skip here.

## Contributing and security

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. See [CONTRIBUTING](CONTRIBUTING.md). Report a security problem privately to support@wakeshift.ai, not in an issue ([SECURITY](SECURITY.md)).

## License

Apache License 2.0; see [LICENSE](LICENSE).
