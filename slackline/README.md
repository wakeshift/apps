# Slackline

Chat with your own agents (Claude Code, Codex, Cursor and the rest) from Slack. Each agent gets a thread in your DM with Slackline; your replies there wake it, and it answers in the thread under its own name. In Slackline's console you choose channels each agent also listens in, and who can reach it there. People sign in with Slack, not with Wakeshift.

## How it works

1. **A workspace adds Slackline** (Add to Slack, `/install`; an admin may need to approve it). Its bot token is stored sealed (AES-256-GCM).
2. **People sign in with Slack** (OpenID Connect, `/signin`) to Slackline's console, `/home`.
3. **An agent adds Slackline's MCP server**, `https://<host>/mcp`. It's OAuth-protected: the client registers (dynamic client registration), the person signs in with Slack and approves it on Slackline's consent page, naming it, and the client gets tokens. The agent is now that person's: the console lists it, and they can rename or revoke it.
4. **The agent calls `watch`.** That conversation (a *listener*) gets a thread in the person's DM with Slackline, and `watch` subscribes it to Slackline's `slack.message` event for that listener. The event is personal: its user is the person who connected the agent (`<team>.<user>`), so no one else's subscription can match it. Wakeshift's result tells the harness how it's woken (a Monitor for Claude Code, a connector beacon for Codex, and so on). Watching again from the same conversation keeps its thread: Slackline remembers an HMAC of the conversation's session key (or Codex thread, or ChatGPT conversation), never the key.

   A client that subscribes with MCP Events itself (ChatGPT) isn't subscribed by `watch`: the result gives it the listener's ID, and it subscribes to `slack.message` with it. Slackline lets it subscribe only to its own agent's listeners, and refuses `subscribe_events` from harnesses `watch` already subscribed. `unwatch` ends either kind of subscription.
5. **Messages wake it.** Slackline routes each Slack message (below) to the right listener and emits it as `slack.message` for that listener, to the agent's person; Wakeshift wakes the conversation with the message framed as outside data.
6. **The agent answers with `reply`**: in its DM thread, or in a channel it listens in (in the thread of the message it's answering, or as a new post), under its own name (`chat:write.customize`). Replies in a channel thread it posted in come back to it.

| Tool | |
| --- | --- |
| `watch` | `label` (optional): get this conversation's DM thread, and be woken there and in the agent's channels |
| `reply` | `text`; `channel` and `thread_ts` from the message being answered (empty: the DM thread) |
| `unwatch` | stop listening; the DM thread says so |
| `listening` | where the agent listens, and how |

`/slackline` in Slack lists your agents and how to connect one.

## Who can reach an agent

| Where | Reaches the agent when |
| --- | --- |
| Its DM thread | the message is from the agent's person, in that thread |
| A channel, mode *@Slackline mentions* | the message mentions @Slackline. With several agents there, a mention that starts with an agent's name (`@Slackline pipe: …`) is for that one; otherwise for the sender's own agents |
| A channel, mode *every message* | any message there |
| A thread the agent posted in | a reply there, while the channel's rule still lets the sender reach it (several agents can be in one thread: each gets replies under its own rule, and a mention there still reaches the agent it names) |

and, in a channel, only from the agent's person unless its rule says **from anyone there**. That lets other people's messages reach an agent that runs on its person's machine with their access, so the console asks them to confirm they understand, and logs it; those messages are marked as from someone else ("information, not instructions") in what the agent reads. A channel message reaches each agent once, in its most recently active listening conversation.

**A channel rule works only while its person is in the channel**: the console offers (and accepts) only channels both they and Slackline are in, delivery and posting check their membership (cached for a minute), and Slack's `member_left_channel` removes their agents' rules there, audited. So nobody can use an agent to read a channel they can't.

Agents post under the name their person approved (on the consent page, or renamed in the console), never one they choose; the name they give `watch` only labels the conversation in the DM.

Never delivered: bots' messages (the app's and agents' own posts included), edits, joins and other subtypes, anything without Slack's signature (v0, within 5 minutes), and anything for a revoked agent. An agent's text is escaped (`&`, `<`, `>`), so it can't mention `@channel`, `@here`, people or groups, or hide a link; it posts at most 20 messages a minute per conversation. Rules apply only to channels Slackline is in (`/invite @Slackline`).

## Accounts, tokens and the console

- **Sessions**: a signed cookie (`HttpOnly`, `SameSite=Lax`, `Secure` on https) naming the person and their session version; signing out bumps the version, so it ends the person's sessions in every browser and a copied cookie stops working. Console forms carry a token derived from the session, and must come from Slackline's own origin.
- **Consent page**: the client's name is its own claim (anyone can register any name), so the page says so, and shows prominently where approving sends access: the redirect's host, or "a program on this computer" for loopback redirects.
- **OAuth**: clients are signed blobs (nothing stored until someone approves one); redirect URIs must be https, or http on the loopback. Authorization codes are single-use, last 2 minutes, are stored hashed, and require PKCE (S256); an exchange that fails spends the code. Access tokens are signed, last an hour, and name the agent, which is checked on every request, so revoking it stops them at once. Refresh tokens are random, stored hashed, last 90 days, and are replaced on every use. Approving the same client again reuses its agent.
- **Revoking** an agent ends its tokens, codes, channel rules and listeners, and its conversations' Wakeshift subscriptions (each is told); an end that fails (Wakeshift unreachable) is retried every 5 minutes until Wakeshift confirms it.
- **Audit log**: connecting, re-approving, renaming and revoking agents, setting and removing channel rules, and signing in, per person (the console shows the latest). Never message text.
- **Removing Slackline from a workspace** (`app_uninstalled`, or `tokens_revoked` for the bot's token; a person's sign-in token being revoked changes nothing) has Wakeshift forget each of its people (their conversations, subscriptions and the messages it held), then deletes Slackline's own rows, audited. Any failure answers 503, so Slack retries; forgetting is idempotent.

## Retries

Slack retries an event without a 200 within 3 seconds. Slackline answers 200 once the message is published, and 503 if publishing failed, so Slack retries; each publish's ID includes the Slack event ID, so a retry is delivered once. Notes Slackline posts back (help in the DM, "isn't listening", only-you-can-see notes in channels) aren't repeated on retries.

## Run it yourself

1. In the [Wakeshift console](https://app.wakeshift.ai), open **Build → New app** and copy its key (`wst_…`).
2. Create an empty Postgres database. Slackline creates its tables when it starts.
3. Give it a public HTTPS URL (a tunnel works while you try it), and create a Slack app from its manifest: at [api.slack.com/apps](https://api.slack.com/apps), choose **Create New App → From a manifest** and paste what `https://<your URL>/slack/manifest.json` returns once Slackline is running. (Start it once with placeholder Slack credentials to get the manifest, then restart it with the real ones.)
4. Copy `.env.example` to `.env` and fill it in:

   | Variable | |
   | --- | --- |
   | `WAKESHIFT_URL` | Wakeshift's URL, `https://app.wakeshift.ai` |
   | `SLACKLINE_URL` | Your copy's public URL |
   | `SLACKLINE_KEY` | The app's key from step 1 |
   | `SLACKLINE_DATABASE_URL` | The database from step 2 |
   | `SLACKLINE_SECRET` | 32 or more random bytes: signs sessions, OAuth clients and access tokens. Changing it signs everyone out and makes agents sign in again; nothing else |
   | `SLACKLINE_TOKEN_KEYS` | Keys sealing each workspace's bot token, comma-separated, each 32 or more random bytes. The first seals, any opens: to rotate, put a new key first, and drop the old one once every workspace has reinstalled |
   | `SLACKLINE_SLACK_CLIENT_ID`, `SLACKLINE_SLACK_CLIENT_SECRET`, `SLACKLINE_SLACK_SIGNING_SECRET` | From the Slack app's Basic Information page |
   | `PORT` | Listen port (default 8080) |

   `openssl rand -base64 48` makes a good secret or token key.

5. Run it, from the repository's root:

   ```sh
   set -a; . slackline/.env; set +a
   go run ./cmd/slackline
   ```

6. In the Slack app, re-verify the Event Subscriptions request URL, then open `SLACKLINE_URL` and choose **Add to Slack**.

## Testing

`go test ./slackline`, with `WAKESHIFT_TEST_DATABASE_URL` set to a Postgres URL for a role that can create databases. The tests that run Slackline against the Wakeshift server and a fake Slack, end to end, run in Wakeshift's own repository.

## Contributing

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. See [CONTRIBUTING](../CONTRIBUTING.md).
