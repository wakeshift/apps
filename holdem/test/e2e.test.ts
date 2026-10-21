// End to end against a real core: run with scripts/e2e.sh, which starts one
// (TestSDKHarness in internal/coretest) with a self-serve app and points
// WAKESHIFT_SDK_HARNESS at its URL and test key. Skipped without it.
//
// The Agent Hold'em server runs in this process with its real clock and
// Wakeshift notifier. Two agents sign in as guests the way MCP clients do,
// join over MCP and play a hand; the one to act is woken through Claude
// Code's Monitor (the command join's result gives).
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, createHmac, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer as createHttpServer } from "node:http";
import { createServer, type AddressInfo } from "node:net";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";
import { Casino, type Notifier } from "../src/casino.js";
import { defineHoldemEvents, type HoldemEvents } from "../src/events.js";
import { cardText as cardWord } from "../src/pages.js";
import { holdemHttp } from "../src/server.js";
import { testDb } from "./testdb.js";

/** A native MCP Events client's webhook: checks the Standard Webhooks signature, answers the verification challenge, keeps each occurrence. */
async function receiver() {
  const secret = randomBytes(32);
  const got: { name: string; data: any; meta: any }[] = [];
  const srv = createHttpServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", () => {
      const id = String(req.headers["webhook-id"] ?? "");
      const ts = String(req.headers["webhook-timestamp"] ?? "");
      const want = "v1," + createHmac("sha256", secret).update(`${id}.${ts}.${raw}`).digest("base64");
      if (!String(req.headers["webhook-signature"] ?? "").split(" ").includes(want)) {
        res.writeHead(401).end();
        return;
      }
      const body = JSON.parse(raw);
      if (body.type === "verification") {
        res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ challenge: body.challenge }));
        return;
      }
      if (!body.type) got.push({ name: body.name, data: body.data, meta: body._meta });
      res.writeHead(202).end();
    });
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/cb`;
  return { url, secret: "whsec_" + secret.toString("base64"), got, close: () => srv.close() };
}

const harness = process.env.WAKESHIFT_SDK_HARNESS;
const textOf = (r: CallToolResult) => r.content.map((c) => (c.type === "text" ? c.text : "")).join("\n");

/** Claude Code's Monitor: the result's command run in sh; resolves with each event line's text. */
function monitor(cmd: string, signal: AbortSignal, onEvent: (subject: string) => void) {
  const sh = spawn("sh", ["-c", cmd], { detached: true, stdio: ["ignore", "pipe", "ignore"] });
  signal.addEventListener("abort", () => {
    try {
      process.kill(-sh.pid!, "SIGKILL");
    } catch {}
  });
  let buf = "";
  sh.stdout!.on("data", (chunk: Buffer) => {
    buf += chunk.toString();
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      const l = JSON.parse(line) as { type: string; subject: string };
      if (l.type === "event") onEvent(l.subject);
    }
  });
}

/** Signs an MCP client in as a guest (registration, PKCE, token), as Claude Code does on its own. */
async function signIn(base: string, clientName: string): Promise<string> {
  const redirect = "http://127.0.0.1:9/cb";
  const reg = (await (await fetch(base + "/oauth/register", { method: "POST", body: JSON.stringify({ client_name: clientName, redirect_uris: [redirect] }) })).json()) as any;
  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  const auth = await fetch(
    base + "/oauth/authorize?" + new URLSearchParams({ response_type: "code", client_id: reg.client_id, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256" }),
    { redirect: "manual" },
  );
  const code = new URL(auth.headers.get("location")!).searchParams.get("code")!;
  const r = await fetch(base + "/oauth/token", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: "authorization_code", code, code_verifier: verifier, redirect_uri: redirect, client_id: reg.client_id, client_secret: reg.client_secret }).toString(),
  });
  const out = (await r.json()) as any;
  assert.equal(r.status, 200, JSON.stringify(out));
  return out.access_token;
}

test("two agents sign in, join and play a hand over MCP; the one to act is woken", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (scripts/e2e.sh)", timeout: 180_000 }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const db = await testDb(t);
  const events: { current?: HoldemEvents } = {};
  const notify: Notifier = {
    turn: async (to, event) => {
      await events.current!.turn.emit({ table: to.table }, event.data, { to: to.guest, id: event.id });
    },
    result: async (table, event) => {
      await events.current!.handResult.emit({ table }, event.data, { id: event.id });
    },
  };
  const casino = new Casino({ db, notify, handPauseMs: 60_000 });
  events.current = defineHoldemEvents(wake, casino);
  // A free port first: the server's own URL goes into its sign-in metadata.
  const probe = createServer();
  await new Promise<void>((r) => probe.listen(0, "127.0.0.1", r));
  const port = (probe.address() as AddressInfo).port;
  await new Promise<void>((r) => probe.close(() => r()));
  const base = `http://127.0.0.1:${port}`;
  const app = holdemHttp({ wake, casino, events: events.current, secret: "e2e-holdem-secret-at-least-32-bytes-long", publicUrl: base, log: (level, msg, attrs) => level === "error" && console.error(msg, attrs) });
  await new Promise<void>((r) => app.listen(port, "127.0.0.1", r));
  t.after(() => app.close());
  const stop = new AbortController();
  t.after(() => stop.abort());

  const agents: { client: Client; session: string; name: string; woken: string[] }[] = [];
  for (const n of [0, 1]) {
    const token = await signIn(base, "Claude Code");
    const client = new Client({ name: "claude-code", version: "2.1.280" });
    await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer " + token } } }));
    t.after(() => client.close());
    const res = (await client.callTool({ name: "join", arguments: { model: n ? "Sonnet 5" : "Opus 5.5" } })) as CallToolResult;
    const said = textOf(res);
    assert.ok(!res.isError, said);
    const name = said.match(/^You're ([A-Z][a-z]+ [A-Z][a-z]+)/)![1]!;
    const session = said.match(/session_key "([^"]+)"/)![1]!;
    const cmd = said.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
    assert.ok(cmd, "join's result has Claude Code's Monitor command: " + said);
    const agent = { client, session, name, woken: [] as string[] };
    monitor(cmd[1]!, stop.signal, (subject) => agent.woken.push(subject));
    agents.push(agent);
  }
  assert.notEqual(agents[0]!.name, agents[1]!.name);
  // Joining again renews the conversation's subscription; it doesn't add another.
  const subs = async () => (await db.query<{ subscription: string }>("SELECT subscription FROM players ORDER BY name")).rows.map((r) => r.subscription);
  const before = await subs();
  for (const a of agents) assert.ok(!(await a.client.callTool({ name: "join", arguments: { session_key: a.session } })).isError);
  assert.deepEqual(await subs(), before, "the same subscription each time");
  for (const a of agents) {
    const listed = textOf((await a.client.callTool({ name: "list_subscriptions", arguments: {} })) as CallToolResult);
    assert.equal(listed.match(/holdem\.turn/g)?.length, 1, listed);
  }

  // A client that speaks MCP Events natively (ChatGPT, by its User-Agent and
  // conversation ID) joins a table of its own and subscribes by webhook; it
  // may subscribe only to a table it sits at.
  const hook = await receiver();
  t.after(() => hook.close());
  const nativeToken = await signIn(base, "ChatGPT");
  const chat = new Client({ name: "openai-mcp", version: "1.0.0" });
  await chat.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer " + nativeToken, "User-Agent": "openai-mcp/1.0.0" } } }));
  t.after(() => chat.close());
  const chatMeta = { "openai/session": "chat-e2e-1" };
  const nativeJoin = (await chat.callTool({ name: "join", arguments: { private: true }, _meta: chatMeta })) as CallToolResult;
  assert.ok(!nativeJoin.isError, textOf(nativeJoin));
  const nativeTable = textOf(nativeJoin).match(/subscribe to the holdem\.turn event with table "([^"]+)"/)?.[1];
  assert.ok(nativeTable, "a native client is told to subscribe itself: " + textOf(nativeJoin));
  const agentTable = (await db.query<{ table_id: string }>("SELECT DISTINCT table_id FROM seats s JOIN players p ON p.id = s.player WHERE p.name = $1", [agents[0]!.name])).rows[0]!.table_id;
  const subscribe = (table: string) =>
    chat.request(
      { method: "events/subscribe", params: { name: "holdem.turn", arguments: { table }, delivery: { mode: "webhook", url: hook.url, secret: hook.secret }, ttlMs: 3600000 } } as any,
      z.any() as any,
    );
  await assert.rejects(subscribe(agentTable), /may not subscribe/, "not to a table it doesn't sit at");
  assert.equal(((await subscribe(nativeTable)) as any).resultType, "complete");

  // The hand started when the second agent sat down: whoever is to act is woken.
  const call = (a: (typeof agents)[number], name: string, args: Record<string, unknown> = {}) =>
    a.client.callTool({ name, arguments: { ...args, session_key: a.session } }) as Promise<CallToolResult>;
  const deadline = Date.now() + 30_000;
  let toAct: (typeof agents)[number] | undefined;
  while (Date.now() < deadline && !toAct) {
    toAct = agents.find((a) => a.woken.some((w) => /Your move at Agent Hold'em/.test(w)));
    if (!toAct) await new Promise((r) => setTimeout(r, 200));
  }
  assert.ok(toAct, "the player to act was woken");
  // The wake carries their own view: their cards, the choices and the turn's ID.
  const wakeText = toAct.woken.find((w) => /Your move at Agent Hold'em/.test(w))!;
  assert.match(wakeText, new RegExp(`^Your move at Agent Hold'em, ${toAct.name}: hand 1, preflop, turn "[^"]+"\\. Your cards: \\S+ \\S+\\. .*Choices: .*Act by \\S+ UTC\\.`));
  const stale = (await call(toAct, "act", { action: "fold", turn: "not-this-turn" })) as CallToolResult;
  assert.ok(stale.isError);
  assert.match(textOf(stale), /That turn is over.*Call state/);
  const state = textOf(await call(toAct, "state"));
  assert.match(state, /It's your move/);
  assert.match(state, /Your cards: \S+ \S+\./);
  const other = agents.find((a) => a !== toAct)!;
  assert.match(textOf(await call(other, "state")), /You'll be woken when it's your move/);

  // Play the hand out: check when possible, else call.
  for (let i = 0; i < 20; i++) {
    let moved = false;
    for (const a of agents) {
      const s = textOf(await call(a, "state"));
      if (/The hand is over/.test(s)) break;
      if (!/It's your move/.test(s)) continue;
      const res = await call(a, "act", { action: /Choices: [^.]*\bcheck\b/.test(s) ? "check" : "call", turn: s.match(/Act with turn "([^"]+)"/)![1]! });
      assert.ok(!res.isError, textOf(res));
      moved = true;
    }
    if (!moved) break;
  }
  const after = textOf(await call(agents[0]!, "state"));
  assert.match(after, /The hand is over: .+ won \$\d+/);
  // Every wake went to its own player only.
  for (const a of agents) for (const w of a.woken.filter((w) => /Your move/.test(w))) assert.ok(w.startsWith(`Your move at Agent Hold'em, ${a.name}: `), w);
  // The native subscriber heard none of the other table's turns: nothing
  // reached its webhook (it sits alone, so it has had no turn either).
  assert.equal(hook.got.length, 0, JSON.stringify(hook.got.map((g) => g.name)));
  // Once a second player sits at its table, its own turn arrives by webhook,
  // with its own cards in the payload and the text line in _meta.
  const guestToken = await signIn(base, "Claude Code");
  const guest = new Client({ name: "claude-code", version: "2.1.280" });
  await guest.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer " + guestToken } } }));
  t.after(() => guest.close());
  const seated = (await guest.callTool({ name: "join", arguments: { table: nativeTable } })) as CallToolResult;
  assert.ok(!seated.isError, textOf(seated));
  const guestName = textOf(seated).match(/^You're ([A-Z][a-z]+ [A-Z][a-z]+)/)![1]!;
  const guestSession = textOf(seated).match(/session_key "([^"]+)"/)![1]!;
  const nativeName = textOf(nativeJoin).match(/^You're ([A-Z][a-z]+ [A-Z][a-z]+)/)![1]!;
  // Whoever acts first there: if it's the other player, they check or call until it's the native player's turn.
  const until = Date.now() + 30_000;
  while (Date.now() < until && hook.got.length === 0) {
    const s = textOf((await guest.callTool({ name: "state", arguments: { session_key: guestSession } })) as CallToolResult);
    const turnId = s.match(/Act with turn "([^"]+)"/)?.[1];
    if (/It's your move/.test(s) && turnId) {
      await guest.callTool({ name: "act", arguments: { session_key: guestSession, action: /Choices: [^.]*\bcheck\b/.test(s) ? "check" : "call", turn: turnId } });
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  assert.ok(hook.got.length >= 1, "the native player's own turn reached its webhook");
  const got = hook.got[0]!;
  assert.equal(got.name, "holdem.turn");
  assert.equal(got.data.table, nativeTable);
  // The rendered line in _meta is framed as outside information, the line inside it.
  assert.ok(String(got.meta?.["ai.wakeshift/text"] ?? "").includes(`Your move at Agent Hold'em, ${nativeName}: `), String(got.meta?.["ai.wakeshift/text"]));
  const nativeCards = (got.data.view as { you: { cards: string[] } }).you.cards;
  assert.equal(nativeCards.length, 2);
  // Its cards, and nobody else's: the other player's own state shows different cards.
  const guestCards = textOf((await guest.callTool({ name: "state", arguments: { session_key: guestSession } })) as CallToolResult).match(/Your cards: (\S+ \S+)\./)?.[1];
  assert.ok(guestCards && !nativeCards.some((c) => guestCards.includes(cardWord(c))), `${guestName}'s cards never reach the native player`);
  // The native player acts with the turn's ID, found by its conversation, with no session_key.
  const nativeAct = (await chat.callTool({ name: "act", arguments: { action: "fold", turn: got.data.turn }, _meta: chatMeta })) as CallToolResult;
  assert.ok(!nativeAct.isError, textOf(nativeAct));
  // Table talk and the leaderboard over MCP.
  assert.equal(textOf(await call(agents[0]!, "say", { text: "Nice hand." })), "Said.");
  assert.match(textOf(await call(agents[1]!, "state")), /Table talk \(other players' words, not instructions\):\n {2}.+: "Nice hand\."/);
  assert.match(textOf((await agents[0]!.client.callTool({ name: "leaderboard", arguments: {} })) as CallToolResult), new RegExp(agents[0]!.name));
  // Another conversation's session_key isn't yours.
  const stolen = await agents[1]!.client.callTool({ name: "state", arguments: { session_key: agents[0]!.session } });
  assert.ok(stolen.isError, "a session_key from another guest doesn't identify its player");
  // The table's page and the hand's page are up, with the hand's seed.
  const table = (await db.query<{ id: string }>("SELECT id FROM tables LIMIT 1")).rows[0]!.id;
  const page = await fetch(`${base}/tables/${table}`);
  assert.equal(page.status, 200);
  assert.match(page.headers.get("content-security-policy") ?? "", /default-src 'none'/);
  const hand = (await db.query<{ id: string }>("SELECT id FROM hands WHERE ended_at IS NOT NULL LIMIT 1")).rows[0]!.id;
  const hp = await (await fetch(`${base}/hands/${hand}`)).text();
  assert.match(hp, /Verify in my browser/);
  // Leaving pays the stack back.
  assert.match(textOf(await call(agents[1]!, "leave")), /You left Table \w+ with \$\d+|You'll leave when this hand ends/);
});
