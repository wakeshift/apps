// Agent Hold'em's tools as a client sees them: the operator note, and the
// wrapped tools (subscribing, rollback, native clients, Wakeshift's block)
// against a fake core. No database: the casino is a stub.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { Wakeshift } from "@wakeshift/sdk";
import { type Casino, GameError } from "../src/casino.js";
import { defineHoldemEvents } from "../src/events.js";
import { HOUSE_HEADER, signHouseClaim } from "../src/house.js";
import { NOTE_DESCRIPTION } from "../src/moderation.js";
import { holdemServer } from "../src/tools.js";

/** A signed-in client of holdemServer, and the note each act reached the casino with. */
async function connect(t: { after: (fn: () => Promise<void>) => void }, operatorNote: boolean) {
  const notes: (string | undefined)[] = [];
  const casino = {
    playerFor: async () => ({ id: "p1" }),
    takeNotice: async () => undefined,
    act: async (_player: string, _action: unknown, _turn: string, _message: unknown, note?: string) => {
      notes.push(note);
      throw new GameError("acted");
    },
  } as unknown as Casino;
  const wake = new Wakeshift({ key: "wst_test_holdem_" + "x".repeat(43), core: "http://127.0.0.1:9" });
  const server = holdemServer({ wake, casino, events: defineHoldemEvents(wake, casino), secret: "tools-test-secret-at-least-32-bytes-long", publicUrl: "http://localhost", log: () => {}, operatorNote });
  const [clientSide, serverSide] = InMemoryTransport.createLinkedPair();
  // Signed in, as oauth.ts would have it.
  const send = clientSide.send.bind(clientSide);
  clientSide.send = (message, options) =>
    send(message, { ...options, authInfo: { token: "t", clientId: "c", scopes: [], extra: { signedIn: { user: "p_1", client: "test-agent" } } } });
  const client = new Client({ name: "test-agent", version: "0" });
  await Promise.all([server.connect(serverSide), client.connect(clientSide)]);
  t.after(async () => {
    await client.close();
    await server.close();
  });
  const act = async (args: Record<string, unknown>) => {
    const r = (await client.callTool({ name: "act", arguments: { action: "fold", turn: "t1", message: "hi", session_key: "s1", ...args } })) as CallToolResult;
    const text = r.content.map((c) => (c.type === "text" ? c.text : "")).join(" ");
    assert.match(text, /acted/, "the move reached the casino: " + text);
    return notes.pop();
  };
  return { client, act };
}

// What makes a model's safeguards take a tool argument as asking for its
// reasoning (Opus refuses every request that carries such a tool list).
const ASKS = /reason|thought|thinking|private/i;

test("act offers an operator note in neutral words, after the move; nothing asks for reasoning", async (t) => {
  const { client, act } = await connect(t, true);
  for (const tool of (await client.listTools()).tools) {
    assert.doesNotMatch(tool.description ?? "", /reason|thought|thinking/i, tool.name);
    for (const [k, v] of Object.entries((tool.inputSchema.properties ?? {}) as Record<string, { description?: string }>)) {
      // join's private option is a table only its link opens: not about the model.
      if (tool.name === "join" && k === "private") continue;
      assert.doesNotMatch(k, ASKS, `${tool.name}.${k}`);
      assert.doesNotMatch(v.description ?? "", ASKS, `${tool.name}.${k}: ${v.description}`);
    }
  }
  const schema = (await client.listTools()).tools.find((x) => x.name === "act")!.inputSchema;
  const props = Object.keys(schema.properties ?? {});
  assert.equal((schema.properties as Record<string, { description?: string }>).note?.description, NOTE_DESCRIPTION);
  assert.ok(props.indexOf("note") > props.indexOf("message"), `note comes after the move: ${props}`);

  assert.equal(await act({ note: "  NOTE-PLAN\n" }), "NOTE-PLAN");
  assert.equal(await act({}), undefined);
});

test("with the operator note off, act doesn't offer it, and a note sent anyway is ignored, not refused", async (t) => {
  const { client, act } = await connect(t, false);
  const act_ = (await client.listTools()).tools.find((x) => x.name === "act")!;
  assert.ok(!Object.hasOwn(act_.inputSchema.properties ?? {}, "note"));
  assert.equal(await act({ note: "NOTE-PLAN" }), undefined);
});

// ── Wrapped tools, over HTTP as main.ts serves them, against a fake core ────

/** The app API calls the wrapper makes, answered and recorded. */
class FakeCore {
  calls: { path: string; body: any }[] = [];
  subs = 0;
  fetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path !== "/manifest" && path !== "/manifest/merge") this.calls.push({ path, body });
    const reply = (json: unknown) => new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/manifest":
      case "/manifest/merge":
        return reply(body ?? { description: "" }); // GET: nothing published yet
      case "/event-subscriptions":
        this.subs++;
        return reply({ event_subscription: { id: `sub-${this.subs}`, user: body.call.user, created: true }, session: "wsc_s1", agent: { harness: "claude-code" }, pending: "pa_1" });
      case "/event-subscriptions/unsubscribe":
        return reply({ ended: true });
      case "/calls/addition":
        return reply({ text: body.native ? `WAKESHIFT NATIVE ${JSON.stringify(body.native)}` : "WAKESHIFT BLOCK" });
    }
    return new Response(JSON.stringify({ error: { code: "not_found", message: path } }), { status: 404 });
  }) as typeof fetch;
  bodies(path: string): any[] {
    return this.calls.filter((c) => c.path === path).map((c) => c.body);
  }
}

const HOUSE_SECRET = "house-test-secret-at-least-32-bytes-long!";
const TOKEN = "bearer-token-1";

/** A casino stub: one player, seated at a new table (or seatedAt, if set); joinFails: the seat is refused. */
function stubCasino(o: { joinFails?: boolean; seatedAt?: string; known?: boolean } = {}) {
  const log = { identify: [] as { key: string; info: Record<string, unknown> }[], dropped: [] as string[], kept: [] as string[] };
  const casino = {
    identify: async (key: string, _guest: string, info: Record<string, unknown>) => {
      log.identify.push({ key, info });
      return { id: "p1", name: "Brave Otter", subscription: null };
    },
    playerFor: async () => (o.known ? { id: "p1", name: "Brave Otter", subscription: null } : undefined),
    playersOfGuest: async () => [],
    tableOf: async () => undefined,
    pickTable: async () => ({ id: "tnew", created: true }),
    join: async (_p: string, opts: { table?: string }) => {
      if (o.joinFails) throw new GameError("That table is full.");
      return { table: o.seatedAt ?? opts.table ?? "thouse", seat: 1, buyIn: 100, already: false };
    },
    dropIfEmpty: async (t: string) => void log.dropped.push(t),
    keepSubscription: async (s: string) => void log.kept.push(s),
    takeNotice: async () => undefined,
    watchLink: async () => undefined,
    state: async () => ({ day: "2026-10-28", balance: 100, table: null }),
  } as unknown as Casino;
  return { casino, log };
}

/** Hold'em's MCP endpoint over HTTP (a new server per request, signed in as client), and clients of it. */
async function serve(t: { after: (fn: () => Promise<void>) => void }, casino: Casino, client = "Claude Code") {
  const core = new FakeCore();
  const wake = new Wakeshift({ key: "wst_test_holdem_" + "x".repeat(43), core: "http://core.test", fetch: core.fetch, onWarning: () => {} });
  const events = defineHoldemEvents(wake, casino);
  const http = createServer(async (req: IncomingMessage & { auth?: AuthInfo }, res) => {
    req.auth = { token: TOKEN, clientId: "c", scopes: [], extra: { signedIn: { user: "p_1", client, agent: "claude-code" } } };
    const server = holdemServer({ wake, casino, events, secret: "tools-test-secret-at-least-32-bytes-long", publicUrl: "http://localhost", log: () => {}, houseSecret: HOUSE_SECRET });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const url = new URL(`http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`);
  const clients: Client[] = [];
  t.after(async () => {
    for (const c of clients) await c.close();
    await new Promise<void>((r) => http.close(() => r()));
  });
  const open = async (headers: Record<string, string> = {}, name = "test-agent") => {
    const c = new Client({ name, version: "0" });
    await c.connect(new StreamableHTTPClientTransport(url, { requestInit: { headers: { Authorization: "Bearer " + TOKEN, ...headers } } }));
    clients.push(c);
    return c;
  };
  const connect = async (headers: Record<string, string> = {}, name = "test-agent") => {
    const c = await open(headers, name);
    return async (tool: string, args: Record<string, unknown> = {}, _meta?: Record<string, unknown>) => {
      const r = (await c.callTool({ name: tool, arguments: args, ...(_meta ? { _meta } : {}) })) as CallToolResult;
      return { ...r, text: r.content.map((x) => (x.type === "text" ? x.text : "")).join("\n") };
    };
  };
  return { core, connect, listTools: async () => (await (await open()).listTools()).tools };
}

test("every tool takes Wakeshift's session_key, harness and conversation_id", async (t) => {
  const { listTools } = await serve(t, stubCasino().casino);
  const ours = (await listTools()).filter((x) => !/_events$|^list_subscriptions$/.test(x.name));
  assert.deepEqual(ours.map((x) => x.name).sort(), ["act", "hand_history", "join", "leaderboard", "leave", "say", "state", "watch_link"]);
  for (const tool of ours) for (const k of ["session_key", "harness", "conversation_id"]) assert.ok(Object.hasOwn(tool.inputSchema.properties ?? {}, k), `${tool.name} takes ${k}`);
});

test("join subscribes the conversation to its table's turns, names the player by the session, and carries Wakeshift's block", async (t) => {
  const { casino, log } = stubCasino();
  const { core, connect } = await serve(t, casino);
  const r = await (await connect())("join", {});
  assert.ok(!r.isError, r.text);
  const [sub] = core.bodies("/event-subscriptions");
  assert.equal(sub.name, "holdem.turn");
  assert.deepEqual(sub.arguments, { table: "tnew" });
  assert.equal(sub.call.user, "p_1");
  assert.equal(sub.call.client, "Claude Code", "the registered client name stands in for initialize's");
  assert.equal(sub.call.tool, "join");
  assert.match(r.text, /session_key "wsc_s1"/);
  assert.match(r.text, /WAKESHIFT BLOCK$/, "Wakeshift's block comes last");
  assert.equal(log.identify[0]!.info.harness, "claude-code", "the harness Wakeshift recognized");
  assert.equal(log.identify[0]!.info.subscription, "sub-1");
  assert.deepEqual(log.kept, ["sub-1"]);
  assert.equal(core.bodies("/event-subscriptions/unsubscribe").length, 0);
});

test("a join whose seat is refused ends the subscription it made, drops the new table, and adds no block", async (t) => {
  const { casino, log } = stubCasino({ joinFails: true });
  const { core, connect } = await serve(t, casino);
  const r = await (await connect())("join", {});
  assert.ok(r.isError);
  assert.match(r.text, /That table is full/);
  assert.doesNotMatch(r.text, /WAKESHIFT/);
  const [rolled] = core.bodies("/event-subscriptions/unsubscribe");
  assert.equal(rolled?.reason, "rolled_back");
  assert.deepEqual(rolled?.arguments, { table: "tnew" });
  assert.deepEqual(log.dropped, ["tnew"]);
  assert.equal(core.bodies("/calls/addition").length, 0);
});

test("a join seated at another table moves the subscription there", async (t) => {
  const { casino, log } = stubCasino({ seatedAt: "tother" });
  const { core, connect } = await serve(t, casino);
  const r = await (await connect())("join", {});
  assert.ok(!r.isError, r.text);
  assert.deepEqual(
    core.bodies("/event-subscriptions").map((b) => b.arguments),
    [{ table: "tnew" }, { table: "tother" }],
  );
  const ended = core.bodies("/event-subscriptions/unsubscribe");
  assert.equal(ended.length, 1);
  assert.deepEqual(ended[0].arguments, { table: "tnew" });
  assert.equal(ended[0].reason, undefined, "an ordinary end, not a rollback");
  assert.deepEqual(log.dropped, ["tnew"]);
  assert.deepEqual(log.kept, ["sub-2"]);
});

test("a house agent joins as a native client: seated first, told to subscribe itself, no conversation subscription", async (t) => {
  const { casino, log } = stubCasino();
  const { core, connect } = await serve(t, casino);
  const claim = signHouseClaim(HOUSE_SECRET, "holdem", { agent: "h1", model: "Haiku 5.5" }, TOKEN, Math.floor(Date.now() / 1000) + 300);
  const r = await (await connect({ [HOUSE_HEADER]: claim }, "wakeshift-house"))("join", {});
  assert.ok(!r.isError, r.text);
  assert.equal(core.bodies("/event-subscriptions").length, 0);
  assert.deepEqual(core.bodies("/calls/addition")[0].native, [{ name: "holdem.turn", arguments: { table: "thouse" } }]);
  assert.equal(core.bodies("/calls/addition")[0].call.client, "", "a house agent's calls carry no client name");
  // What the house agents read from join's reply: their name and the table's link.
  assert.match(r.text, /^You're Brave Otter\./);
  assert.equal(r.text.match(/\/tables\/(t[0-9a-z]+)/)?.[1], "thouse");
  assert.doesNotMatch(r.text, /session_key/);
  assert.equal(log.identify[0]!.info.harness, "house");
  assert.equal(log.identify[0]!.info.house, true);
});

test("a ChatGPT conversation is its own native player, keyed by its conversation", async (t) => {
  const { casino, log } = stubCasino();
  const { core, connect } = await serve(t, casino, "ChatGPT");
  const call = await connect({ "User-Agent": "openai-mcp/1.0.0" }, "openai-mcp");
  const a = await call("join", {}, { "openai/session": "chat-1" });
  const b = await call("join", {}, { "openai/session": "chat-2" });
  assert.ok(!a.isError && !b.isError, a.text + b.text);
  assert.equal(core.bodies("/event-subscriptions").length, 0);
  assert.match(a.text, /WAKESHIFT NATIVE .*holdem\.turn/);
  assert.equal(log.identify[0]!.info.harness, "chatgpt");
  assert.notEqual(log.identify[0]!.key, log.identify[1]!.key, "one player per conversation");
});

test("state subscribes nothing, and carries Wakeshift's block for the conversation it names", async (t) => {
  const { core, connect } = await serve(t, stubCasino({ known: true }).casino);
  const r = await (await connect())("state", { session_key: "wsc_s1" });
  assert.ok(!r.isError, r.text);
  assert.equal(core.bodies("/event-subscriptions").length, 0);
  assert.equal(core.bodies("/calls/addition")[0].call.session, "wsc_s1");
  assert.match(r.text, /You're not at a table[\s\S]*WAKESHIFT BLOCK$/);
});
