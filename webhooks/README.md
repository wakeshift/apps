# Webhooks

A webhook URL that wakes your agent: get a link, add it to your agent (Claude Code, Codex, Cursor, Antigravity, opencode, Copilot CLI or any MCP client), ask the agent to watch, and every POST to your webhook URL (curl, Postman, GitHub, Stripe, Linear, your own app) wakes it. Wakeshift runs it at try.wakeshift.ai. Built on the TypeScript SDK, `@wakeshift/sdk`.

## How it works

- **Stateless links.** A link's code is signed with `TRY_SECRET` and names a random webhook ID and when it was made. The MCP URL (`/t/<code>/mcp`), the page (`/l/<code>`), the webhook URLs (`/in/<token>`, one token per signature check) and the signing secret are all derived from it. Nothing is stored. A link expires after 7 days without use; when it was last used is kept in the database (written at most every 5 minutes per instance). When it expires, Wakeshift is asked to forget the webhook (`forgetUser` with the webhook ID, which is the app's user for it): its workspace, the conversations that watched it, their subscriptions and deliveries. The sweep queues it in the database in the transaction that deletes the link's own rows, and every instance works the queue every 10 minutes, retrying a failure with backoff (a minute, doubling, at most 6 hours) until Wakeshift says it's done.
- **The SDK user is the webhook; posts are an event.** `webhook.received` (argument: the webhook's ID; only the webhook's own SDK user, anyone with its link, may subscribe) carries what was posted: `summary`, a line saying what happened, then the parsed body and the headers that say who sent it. `watch` subscribes the calling conversation with `event.subscribe` and returns `sub.result(…)`; `unwatch` ends it; a client that speaks MCP Events (ChatGPT) is told to subscribe itself. A post becomes `event.emit`. `wake.attach` adds `subscribe_events`, `unsubscribe_events` and `list_subscriptions`. `recent_posts` and `webhook_info` pass their results to `wake.annotate`, which adds pending messages and Codex beacons; nothing else changes a result.
- **MCP:** Streamable HTTP with sessions, no sign-in (the code in the URL is the credential). Nothing about a session lives in an instance: initialize's client name, version and capabilities are stored under the `Mcp-Session-Id`, and each request builds the MCP server afresh from them on whichever instance it reaches (the SDK's transport in stateless mode; the session ID is checked here, 404 if unknown or another link's). So sessions survive restarts and need no affinity, and the client's name (which decides how Cursor, opencode and the rest are woken) is right on every instance. Sessions idle for 7 days are dropped (the client initializes again). Tools: `watch`, `unwatch`, `recent_posts`, `webhook_info`.
- **Webhooks:** `POST`, `PUT` or `PATCH` `/in/<token>`, any body up to 64 KB. The event's `text` says what happened: for GitHub (from `X-GitHub-Event`) `GitHub workflow_run completed: failure on main (tests) in acme/api`; for Stripe `Stripe invoice.payment_failed: in_…, USD 49.00, open (evt_…, test mode)`; for Linear `Linear Issue create: ENG-123 Checkout page 500s`; for anything else `POST to your webhook` (with ` (empty body)` if it was), since the data has the rest: the text doesn't repeat the body, so a small post's data fits on one Monitor line (480 bytes) in full. Its `data` is the parsed body (`body`, or `body_base64`), the method, content type, a few headers that say who sent it (including `Linear-Event` and `Linear-Delivery`; never Authorization, cookies or signatures) and, for Stripe and Linear, `source` (the event type and ID). Retries are one publish: by `Idempotency-Key`, `X-GitHub-Delivery`, `Linear-Delivery`, or a Stripe event's `evt_` ID. On a GitHub, Stripe or Linear URL only the service's own ID counts (`Idempotency-Key` isn't signed, so a captured delivery replayed with a fresh one would wake the agent again); a Stripe URL uses only the `evt_` ID from the signed body. `GET` explains how to post; a body more than 256 KB over the limit has its connection dropped rather than read.
- **Signature checks:** the page's tabs give a URL per check, each its own token, so a checked URL can't be turned into the open one; the open URL keeps working for whoever has it. GitHub's `X-Hub-Signature-256` and `Authorization: Bearer` use secrets derived per webhook and per check (`try_gh_…`, `try_bearer_…`). Stripe and Linear make their own secret: the user pastes it into the page, and it's sealed into that URL's token (AES-256-GCM with a key derived from `TRY_SECRET`). It's stored only in that form: the URL is kept as the webhook's checked URL (below) until the link expires. Stripe's `Stripe-Signature` (`t=…,v1=…`, HMAC-SHA256 of `t.body`) must be within 5 minutes; Linear's `Linear-Signature` (hex HMAC-SHA256 of the body) needs a `webhookTimestamp` within a minute.
- **Which URL the agent hears about:** `watch` and `webhook_info` give the checked URL the user last picked (a GitHub or bearer tab they opened, a Stripe or Linear URL they made, or the URL a service last posted to), and say which check it uses. It's kept for as long as the link lives.
- **Limits** (counted in the database, atomically, so they hold across instances; fixed windows: the hour and the UTC day): 60 posts an hour and 500 a day for each of a webhook's URLs (429 with `Retry-After`), so whoever has the open URL can't use up the checked one's. Refused posts (401, 413) don't count; retries do (each is still a request to Wakeshift, so a repeated `Idempotency-Key` can't get round the limits). 20,000 an hour across all webhooks, 30 new links an hour per address (an IPv6 address by its /64; kept as an HMAC of the address, for the hour). `TRY_PER_HOUR`, `TRY_PER_DAY`, `TRY_GLOBAL_PER_HOUR` and `TRY_LINKS_PER_HOUR` change them, and the terms and privacy policy state the limits in use.
- **Overload:** an instance takes 256 requests in flight (posts three quarters of that, so pages and tool calls keep some room) and 900 open streams (live views, MCP GET streams), at most 20 of them for one webhook and 2 GET streams for one MCP session (a live view past that gets 429, a GET stream 405); past that, and when the database is away or out of connections, it answers 503 with `Retry-After` and a plain message (JSON for webhooks, JSON-RPC for MCP, a page for pages), never a 500. A link's page still comes up when the database is slow, without its history; the live view retries with backoff and says so. Wakeshift's own 429 or 503 on publish becomes a 503 with `Retry-After` for the poster.
- **The page's live view** is SSE from this server (`/l/<code>/live`): each post (received, refused, rate limited, the start of what was posted, and how many watching conversations the publish reached), and each watch and unwatch. Lines are kept in the database (the last 50 per webhook) and reach pages on every instance by `NOTIFY` on the webhook's channel (`try_<hook>`; each instance `LISTEN`s only for webhooks it has a page open for, and replays the recent lines whenever its connection comes back after a drop; it pings that connection every 30 seconds so a silent drop is noticed). A webhook's refused and rate-limited posts get at most 10 lines a minute per instance, so a flood of bad signatures doesn't push real posts out. A live view that doesn't keep up with what's written to it is dropped (EventSource reconnects). The page shows who called `watch`, "sent to N watching conversations" per post, and whether an agent is connected right now and how (stream, hooks or polling): it polls `/l/<code>/status` every 10 seconds, which asks Wakeshift's read-only `topicStatus` for the webhook's topic.

## Run it yourself

1. In the [Wakeshift console](https://app.wakeshift.ai), open **Build → New app** and copy its key (`wst_…`).
2. Create an empty Postgres database. Webhooks creates its tables when it starts (`migrations/`).
3. Build the SDK, then Webhooks, from the repository's root:

   ```sh
   (cd third_party/wakeshift-node && npm ci && npm run build)
   cd webhooks && npm ci && npm run build
   ```

4. Copy `.env.example` to `.env`, fill it in, and run it:

   | Variable | |
   | --- | --- |
   | `WAKESHIFT_KEY` | The app's key from step 1 |
   | `WAKESHIFT_CORE` | Wakeshift's URL (default `https://app.wakeshift.ai`) |
   | `TRY_SECRET` | Signs links; 32 or more random bytes. Changing it ends every link |
   | `TRY_DATABASE_URL` | The database from step 2; every instance shares it |
   | `TRY_DB_POOL` | Connections per instance (default 3), plus one that listens for the live view |
   | `PUBLIC_URL` | Your copy's public URL (default `http://localhost:$PORT`) |
   | `PORT` | Listen port (default 8380) |
   | `TRY_PROXY_HOPS` | Proxies in front of it that append to `X-Forwarded-For` (Google's load balancer: 2); 0 uses the socket's address |
   | `TRY_PER_HOUR`, `TRY_PER_DAY`, `TRY_GLOBAL_PER_HOUR`, `TRY_LINKS_PER_HOUR` | Optional: the limits above |

   ```sh
   set -a; . ./.env; set +a
   npm start
   ```

5. Open `PUBLIC_URL`, get a link, and run the command its tab for your agent shows.

## Testing

```sh
npm run lint
npm test     # unit tests; needs WAKESHIFT_TEST_DATABASE_URL, a Postgres URL for a role that can create databases
```

The end-to-end and load tests run Webhooks against the Wakeshift server, so they run in Wakeshift's own repository.

## Contributing

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. See [CONTRIBUTING](../CONTRIBUTING.md).
