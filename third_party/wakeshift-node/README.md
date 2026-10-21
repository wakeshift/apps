# @wakeshift/sdk

Let agents subscribe to things in your app. Inside one of your MCP tools you subscribe the conversation that called it to a topic; when you publish to that topic, Wakeshift wakes the agent. You don't deal with harnesses: Wakeshift puts what each one needs (Claude Code, Codex, and the rest) into your tool's result.

Node 22 or later. Peer dependencies: `@modelcontextprotocol/sdk` (1.23 or later: the first that takes zod 4) and `zod` (3.25 or later, or 4). Import `z` from `"zod"`, not `"zod/v4"`: `wake.args` is built from `"zod"`, and the MCP SDK refuses a schema that mixes the two.

## Install

The SDK isn't on npm yet. Install it from the tarball that came with your invite, or pack one from this directory:

```sh
# in sdk/typescript (the root of github.com/wakeshift/wakeshift-node)
npm install && npm pack          # writes wakeshift-sdk-0.1.0.tgz

# in your app
npm install /path/to/wakeshift-sdk-0.1.0.tgz @modelcontextprotocol/sdk zod
```

Use the tarball rather than `npm install /path/to/sdk/typescript` (a `file:` dependency). A `file:` dependency links the directory along with its `node_modules`, so the SDK can load its own development copy of zod instead of yours, and the MCP SDK then fails with "Mixed Zod versions detected in object shape". `scripts/pack-check.sh` installs the tarball with zod 3 and with zod 4, each with the oldest MCP SDK it supports and the latest, to check it.

## Quickstart: a CI bot

You have an MCP server and a GitHub webhook handler. Register an app in the Wakeshift console (Build), and put its key in `WAKESHIFT_KEY` and the console's address in `WAKESHIFT_CORE` (for example `https://app.staging.wakeshift.ai`). Load them however you load environment variables, for example `node --env-file=.env server.ts`.

```ts
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY!, core: process.env.WAKESHIFT_CORE })
const server = new McpServer({ name: "ci-bot", version: "1.0.0" })

server.registerTool(
  "watch_ci",
  {
    description: "Wake me when CI finishes on a repo",
    inputSchema: { repo: z.string(), branch: z.string().optional(), ...wake.args },
  },
  async ({ repo, branch, ...args }, extra) => {
    if (!(await canSeeRepo(extra, repo))) throw new Error(`You can't see ${repo}.`)
    const user = extra.authInfo?.extra?.userId as string | undefined // your own ID for the person, if you have one
    const sub = await wake.subscribe(wake.call(extra, { server, user, args }), `ci/${repo.toLowerCase()}`, {
      filter: branch ? { branch } : undefined,
      replay: "5m",
    })
    return sub.result(`Watching CI on ${repo}.`)
  },
)
```

```ts
// In your GitHub webhook handler, after checking X-Hub-Signature-256:
if (req.headers["x-github-event"] === "workflow_run" && body.action === "completed") {
  const run = body.workflow_run
  await wake.publish(`ci/${body.repository.full_name.toLowerCase()}`, {
    id: String(req.headers["x-github-delivery"]),
    text: `CI ${run.conclusion} on ${run.head_branch}: ${run.name}`,
    data: { branch: run.head_branch, url: run.html_url },
  })
}
```

That's all. An agent that calls `watch_ci` is woken with the text and data of each publish that passes its filter, framed as information from your app, not as instructions from its user.

- `...wake.args` adds three optional arguments to this tool, and the SDK touches no other tool or result:
  - `session_key`: Claude Code sends no conversation ID, so the result gives the agent a key to pass back; without it, a second call makes a second session. Keys are random (`wsc_…`), so one conversation can't guess another's, but a key is a bearer credential for its conversation (its description tells the agent so): don't log tool arguments or results that carry it. A key Wakeshift doesn't know (it ended, or the agent mistyped it) starts a new session on `subscribe`, and the result says so; other calls ignore it.
  - `harness`: the program the agent runs in. Some programs' calls look alike (Grok Bot's and Cursor's cloud agents' are Cursor's), so the agent says which it is and gets instructions that work there. It's a hint: it only refines what the call was recognised as, and one that contradicts the client name is ignored.
  - `conversation_id`: the agent's own ID for the conversation, where its program gives one (Cursor and Grok Bot: `CURSOR_CONVERSATION_ID`).
- `wake.call` reads them from `args`, and the client's name from `server`.
- For an agent that can't be woken, messages are held, and the result tells it to call the tool again to check for them. To show them in another tool's results too, spread `...wake.args` into that tool's input and return `wake.annotate(wake.call(extra, { server, args }), result)`.
- Wakeshift tells harnesses apart by the MCP client's name, which the client sends in `initialize`. A server created per request (the stateless Streamable HTTP pattern) never sees `initialize`, so Wakeshift falls back to the tool call's `_meta` and the request's User-Agent, which `wake.call` forwards. That covers Claude Code, Codex, Antigravity, Copilot CLI, opencode and Cursor. For any other client on a stateless server, keep one `McpServer` per MCP session, or pass the name yourself with `wake.call(extra, { client })`. The SDK warns once when Wakeshift recognised nothing and the agent didn't say, and `sub.agent` says what it recognised.
- Your test key works against test workspaces only, which Wakeshift makes for your users. Nothing else changes when live keys arrive.

## Events

Events are the other way to offer subscriptions, following MCP Events (SEP-3415): you define the event types your app offers, serve them, and emit occurrences. Clients that speak MCP Events, such as ChatGPT, subscribe natively; every other agent subscribes with the `subscribe_events` tool `serveEvents` adds, and Wakeshift wakes it its own way. You write no subscribe tool and handle no harnesses.

```ts
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { z } from "zod"
import { Wakeshift, guestOf } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY!, core: process.env.WAKESHIFT_CORE })

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

// One server per connection; serveEvents before connecting it.
const server = new McpServer({ name: "chat", version: "1.0.0" })
wake.serveEvents(server, { user: (extra) => guestOf(extra)?.user })

// When something happens:
await message.emit({ channel: "eng" }, { from: "ada", text: "shipped it" })
await turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "guest:abc123" })
```

- **`wake.defineEvent(name, { description, args, payload, text, authorize?, personal? })`** returns the event. `args` is what a subscriber picks; its required string, integer and boolean fields route occurrences, so their values are lowercase IDs (a-z 0-9 . _ -), and an optional one filters on the payload field of the same name. `authorize(subscriber, args)` decides who may subscribe. Define every event before `serveEvents`.
- **`event.emit(args, payload, { id?, to? })`** checks the payload against its schema and sends it to everyone subscribed with those arguments; a personal event goes to the one subscriber `to` names (the `user` serveEvents gives for them). `id` makes retries emit once.
- **`wake.serveEvents(server, { user? })`** serves `events/list`, `events/subscribe` and `events/unsubscribe` to clients that speak MCP Events, and adds `subscribe_events` (for everyone else), `unsubscribe_events` and `list_subscriptions` (for every client, so a person can always see and stop what they're subscribed to). `user` is the app's ID for the person behind a request; by default, the guest `GuestAuth` signed in. The events go into your manifest on first use; `publishManifest` adds them too.
- **`event.subscribe(extra, args, { server or client, user?, wakeArgs? })`** subscribes the conversation calling one of your own tools, the way `subscribe_events` would, so a tool like "join" can set up its wakes in one step; it gives back the subscription's `id`, the conversation's `session` (its `session_key`) and `result(text)`, your tool's text with what the agent needs added. Your tool decides who may (`authorize` isn't consulted). Clients that speak MCP Events subscribe themselves. **`event.unsubscribe(extra, args, …)`** ends the calling conversation's subscription with those arguments (for a tool like "unwatch"); **`event.end(user, id)`** ends one of a user's subscriptions by its ID.

## Reference

### `new Wakeshift({ key, core?, fetch?, timeoutMs?, onWarning? })`

- `key`: `wst_test_<app>_<seed>`. It's your app's Ed25519 private key; it never goes over the wire. Each request carries a fresh assertion signed with it (valid two minutes).
- `core`: the address of the console the key came from, for example `https://app.staging.wakeshift.ai`. Default: `WAKESHIFT_CORE` from the environment. A test key needs one of the two, and the constructor throws without them; a live key falls back to `https://app.wakeshift.ai`.
- `onWarning(message)`: called once per kind of problem that doesn't fail a call, such as a tool call Wakeshift recognised no harness in. Default: `console.warn`.
- `wake.appId`, `wake.mode` (`"test"` or `"live"`), `wake.core`.

### The caller

- `wake.call(extra, { server, user?, args?, session?, harness?, conversation?, client? })` packs a tool call from an MCP tool callback's `extra`: its `_meta` (Codex sends its thread there), the request's User-Agent (from `extra.requestInfo`), the client's name (from `server`'s client info; or pass `client` yourself, for example on a stateless server), `user` (your ID for the person; their conversations are grouped, and `u/<user>/…` topics are theirs), and `session_key`, `harness` and `conversation_id` from `args`.
- `wake.callOf({ meta, clientName, user?, session?, userAgent?, harness?, conversation? })` does the same without an MCP framework.
- `wake.args`: a zod shape with `session_key`, `harness` and `conversation_id` (`SESSION_ARG`, `HARNESS_ARG`, `CONVERSATION_ARG`), to spread into your subscribe tool's `inputSchema` (and any tool whose results you annotate).

### Topics

Lowercase segments of `a-z 0-9 . _ -` joined by `/`, up to 200 characters, scoped to your app. `u/<user>/…` is private: Wakeshift refuses to subscribe a caller whose `user` isn't `<user>`. So a user with private topics needs an ID that is itself a topic segment (lowercase `a-z 0-9 . _ -`): use an opaque ID, or a hash of yours, not an email. Every other topic is shared, so check access before `subscribe`, and when someone loses access, end their subscriptions with `unsubscribeUser` (or `endSubscription`). Don't put secrets in topic names.

### Calls

| Call | Returns |
| --- | --- |
| `subscribe(call, topic, { filter?, once?, replay?, ttl? })` | `{ id, topic, filter, once, expiresAt, session, addition, agent, result(textOrResult) }`. `agent`: `{ harness, via, claimed }`, what Wakeshift recognised the caller as and by which signal (`"client"`, `"meta"`, `"user_agent"`, or `"claim"` when only the agent's `harness` named it), both `null` when nothing; `claimed` is what the agent said (normalized), or `null`, and one that differs from `harness` was ignored. `filter`: equality on top-level fields of the event's `data`. `once`: end after the first delivery. `replay`: deliver the latest event published within this long before subscribing (at most `"24h"`). `ttl`: end after this long without a delivery or a fresh subscribe (default `"30d"`). Durations are seconds or `"90s"`, `"5m"`, `"2h"`, `"30d"` (whole days). Subscribing again refreshes it. `result` returns your text (or `CallToolResult`) followed by Wakeshift's text, with its `_meta`. |
| `unsubscribe(call, topic?)` | How many of the calling conversation's subscriptions ended (all your topics without `topic`). |
| `unsubscribeUser(user, topic?)` | How many ended: every conversation of `user` stops getting `topic` (all your topics without it). Each conversation is told you ended it. For when someone loses access. |
| `endSubscription(subscriptionId)` | `true` if it ended one subscription (its conversation is told you ended it); `false` if it had already ended or isn't yours. |
| `publish(topics, { text, data?, id? })` | `{ matched, duplicate }`. `topics`: one or up to 20. `text` at most 1000 bytes; `data` a JSON object within the 64 KB request limit. The same `id` within a day publishes once (`duplicate: true`); after a day, it publishes again. |
| `send(subscriptionId, event)` | `"delivered"` (connected, on its way), `"queued"` (waits for the conversation) or `"closed"` (the subscription ended). Ignores the filter. |
| `topicStatus(topic)` | `{ topic, subscriptions, connected, via: { stream, poll, hooks, webhook } }`: how many conversations are subscribed, and how many are connected now and how (`stream`: a stream held open, like Cursor's background command; `poll`: long polls, waits or a wait tool, like Claude Code's Monitor; `hooks`: Wakeshift's hooks or connector on the user's machine; `webhook`: a ChatGPT chat monitoring the event through MCP Events). Only reads. |
| `subscriptionStatus(subscriptionId)` | `{ id, topic, status: "active" \| "closed", connected, via?, expiresAt? }`. Only reads. |
| `publishUrl(topic)` | `{ id, topic, url }`: anyone can POST to `url` to publish to `topic` (a JSON object body becomes `data`; `Idempotency-Key` or `X-GitHub-Delivery` dedupes). |
| `revokePublishUrl(id)` | The URL answers 410 from then on. |
| `annotate(call, result)` | A copy of `result` with what Wakeshift adds for this conversation (a fresh Codex connector beacon, messages waiting; nothing while what it was handed a moment ago is unused). Only results you pass here (and `sub.result`) change. `addition(call)` returns the raw `{ text?, meta?, agent }`. |
| `closeTopic(topic)` | How many subscriptions ended. Each conversation gets what was already published to it, then a notice. |
| `forgetUser(user)` | `false` if there was nothing to forget. Deletes everything Wakeshift keeps for the user: their conversations, subscriptions, events and deliveries, stream links (open streams close), recent publishes to their `u/<user>/…` topics and publish URLs for them, and their ID. Your app's log records it with `user_sha256`, the hex SHA-256 of `"<app>:<user>"`, never the ID. Their subscription IDs stay known as closed. |

### Guest sign-in: `new GuestAuth({ wake, mcpUrl, … })`

An OAuth server inside your app that signs MCP clients in as guests, with no user step (site/docs/users.md has the walkthrough). Each registered client is one guest, with its own workspace and an agent named after its harness.

- `await guests.handle(req, res)`: serves `/.well-known/oauth-protected-resource` (and `…/mcp`), `/.well-known/oauth-authorization-server`, `/oauth/register`, `/oauth/authorize` and `/oauth/token`; `false` for any other request. Works with `node:http` and Express (a body a parser already read is used as it is).
- `guests.authenticate(req, res)`: the guest for the request's bearer token, and `req.auth` set for the MCP transport; or `null` after answering 401 with `WWW-Authenticate: Bearer resource_metadata="…"`.
- `guestOf(extra)` in a tool callback: `{ user, workspace, agent, client, expires }`. Pass `user` and `client` to `wake.call`.
- `guests.verify(token)`: the guest, or `null`.
- Options: `base` (default `mcpUrl` without `/mcp`), `accessTtl` (seconds, default 3600), `refreshTtl` (default 30 days), `revoked(guest, issuedAt)`, `proxyHops`, `registerLimit` (default 20) and `tokenLimit` (default 60) per address per `rateWindow` (default 600 seconds), `grants` (also give each guest's agent a grant for the manifest's agent scope, as the Go SDK does), `onError`.
- A refresh returns a new refresh token; the old one works until it expires (nothing is stored). `wake.forgetUser(guest.user)` refuses all the guest's refreshes from then on, and `revoked` refuses the ones you choose, such as those issued before a cutoff.

### Sign in with Wakeshift

For a person's Wakeshift user ID, the same across sign-ins, devices and agents (for example to link guests to people). Needs a live key, and the redirect URI in the manifest's `agent.redirect_uris`.

- `new WakeshiftSignIn({ wake, redirectUri, cookie? })`: `login.start(res, { link?, agentHint? })` sends the person to Wakeshift (a 302, and a signed HttpOnly cookie holding the PKCE verifier and state for ten minutes); `await login.finish(req, res)` on the redirect URI checks the cookie, the state and the issuer, exchanges the code and returns `{ signedIn, link }`, or throws `SignInError` with `code` `expired`, `state`, `issuer`, `denied` (`detail`: Wakeshift's OAuth error) or `failed`.
- `signedIn`: `{ user, workspace, agent, grant, accessToken, expiresAt, refreshToken, connection? }`.
- The parts, without the cookie: `wake.pkce()`, `wake.authorizeUrl({ redirectUri, state, challenge, agentHint? })`, `wake.signIn({ code, verifier, redirectUri })`, `wake.refresh(refreshToken)` (throws `invalid_grant` once the person revokes your app).

### Manifest and workspaces

- `wake.publishManifest({ description?, accounts?, agent?, workspace? })`: what your app may do; returns it as Wakeshift keeps it. `agent`: `{ wake, post, ttl_days, redirect_uris?, wake_limits?, start_prompt? }`, each scope event `{ pattern, description }`.
- `wake.workspace(user, name)`, `wake.addAgent(workspace, agent, displayName)`, `wake.checkAgent(workspace, agent)`: the workspace Wakeshift keeps for one of your users, an agent in it with a grant for your manifest's agent scope, and whether that grant is live. `GuestAuth` uses these; topics don't need them.
- `harnessAgent(clientName)`: the agent ID a client name maps to (`"Claude Code"` is `claude-code`), as Wakeshift names it.

### Errors

API errors throw `WakeshiftError` with `status` and `code`, the same codes as the HTTP API's (site/docs/http-api.md), plus `unreachable`:

| Status | Code | When |
| --- | --- | --- |
| `400` | `bad_request` | Something in the request is invalid, including a field the API doesn't take. The message says what. |
| `401` | `unauthenticated` | The assertion was refused: expired, reused, signed with another key, or for another `aud` (a key for another Wakeshift). The message says which. |
| `403` | `private_topic` | Subscribing a caller to another user's `u/<user>/…` topic. |
| `403` | `other_client` | The `session` key is a conversation in another MCP client of the same user. Call again without it. |
| `403` | `not_headless` | Topics need an app whose users are its own. |
| `404` | `unknown_subscription` | The subscription ID isn't one of your app's. |
| `404` | `unknown_publish_url` | The publish URL ID isn't one of your app's. |
| `413` | `too_large` | The request body is over 64 KB. The limit is on the whole body, so `data` must leave room for the rest of the request. |
| `0` | `unreachable` | No answer at all: a wrong `core`, a network problem, or the timeout. The message names the core it tried. |

`isCode(err, "private_topic")` checks one.

## Development

```sh
npm install
npm test          # unit tests (node:test)
npm run build     # dist/
make sdk-test     # from the repo root: unit tests, then end to end against a real core (needs Docker for Postgres)
```

The end-to-end test (`test/e2e.test.ts`, run by `scripts/e2e.sh`) starts a core with Go's `TestSDKHarness`, which registers a self-serve app and writes the core's URL and a test key to a file.

## Contributing

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. See [CONTRIBUTING](CONTRIBUTING.md).
