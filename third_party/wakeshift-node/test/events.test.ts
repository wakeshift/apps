import { test } from "node:test";
import assert from "node:assert/strict";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { McpError, type CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { readFileSync } from "node:fs";
import {
  BUILTIN_TOOLS,
  EVENT_TOOL_TEXT,
  EXTENSION_KEY,
  LIST_TOOL_DESCRIPTION,
  SUBSCRIBE_TOOL_DESCRIPTION,
  UNSUBSCRIBE_TOOL_DESCRIPTION,
  Wakeshift,
  WakeshiftError,
  type BuiltinTool,
} from "../src/index.js";
import { speaksEvents } from "../src/events.js";

const seed = Buffer.alloc(32, 9).toString("base64url");
const KEY = `wst_test_chat_${seed}`;

interface Req {
  method: string;
  path: string;
  body: any;
}

// fakeCore answers the app API: each path's reply from replies (a function
// of the body), {} for anything else; it records every request.
function fakeCore(replies: Record<string, (body: any) => { status?: number; json: any }> = {}) {
  const reqs: Req[] = [];
  const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ method: init?.method ?? "GET", path, body });
    const r = replies[path]?.(body) ?? { json: {} };
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { reqs, wake: new Wakeshift({ key: KEY, core: "http://core.test", fetch: fakeFetch }) };
}

function chatEvent(wake: Wakeshift) {
  return wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string(), from: z.string().optional() }),
    payload: z.object({ from: z.string(), text: z.string() }),
    text: (m) => `${m.from}: ${m.text}`,
    authorize: (_s, a) => a.channel !== "secret",
  });
}

function turnEvent(wake: Wakeshift) {
  return wake.defineEvent("game.turn", {
    description: "It's your move",
    args: { table: z.string() },
    payload: z.object({ hole: z.array(z.string()), toCall: z.number() }),
    text: (t) => `Your move: ${t.toCall} to call`,
    personal: true,
  });
}

test("defineEvent makes the manifest's event type: a topic per required argument, a filter per optional one", () => {
  const { wake } = fakeCore();
  const chat = chatEvent(wake);
  assert.equal(chat.manifest.topic, "ev/chat.message/{channel}");
  assert.deepEqual(chat.manifest.filter, { from: "{from}" });
  assert.equal(chat.manifest.arguments.type, "object");
  assert.equal("$schema" in chat.manifest.arguments, false);
  assert.deepEqual(chat.manifest.arguments.required, ["channel"]);
  assert.equal(chat.manifest.personal, undefined);
  const turn = turnEvent(wake);
  assert.equal(turn.manifest.topic, "ev/game.turn/{table}");
  assert.equal(turn.manifest.personal, true);
  assert.deepEqual(turn.definition().delivery, ["webhook", "poll"]);
});

test("a filter given without a topic gets the default's added, and the caller's object stays as it was", () => {
  const { wake } = fakeCore();
  const filter = { room: "{channel}" };
  const ev = wake.defineEvent("chat.room", {
    description: "A message in a room",
    args: z.object({ channel: z.string(), from: z.string().optional() }),
    payload: z.object({ from: z.string(), room: z.string() }),
    text: (m) => m.from,
    filter,
  });
  assert.deepEqual(ev.manifest.filter, { room: "{channel}", from: "{from}" });
  assert.deepEqual(filter, { room: "{channel}" });
});

test("a filter on the field an optional argument already filters is refused, naming the field, whatever its value", () => {
  const { wake } = fakeCore();
  const opts = {
    description: "A message in a room",
    args: z.object({ channel: z.string(), from: z.string().optional() }),
    payload: z.object({ from: z.string(), room: z.string() }),
    text: (m: { from: string }) => m.from,
  };
  for (const tmpl of ["{channel}", "{from}"]) {
    assert.throws(() => wake.defineEvent("chat.room", { ...opts, filter: { from: tmpl } }), /filter on from conflicts with optional argument from/);
  }
  // With a topic there's no default filter, so the same filter is the app's own.
  const own = wake.defineEvent("chat.room", { ...opts, topic: "rooms/{channel}", filter: { from: "{from}" } });
  assert.deepEqual(own.manifest.filter, { from: "{from}" });
});

test("defineEvent refuses what Wakeshift would", () => {
  const { wake } = fakeCore();
  const p = z.object({ x: z.string() });
  const text = () => "x";
  assert.throws(() => wake.defineEvent("Chat", { description: "d", args: {}, payload: p, text }), /lowercase dot-separated/);
  assert.throws(() => wake.defineEvent("wakeshift.message", { description: "d", args: {}, payload: p, text }), /Wakeshift's/);
  assert.throws(() => wake.defineEvent("a.b", { description: "", args: {}, payload: p, text }), /description/);
  assert.throws(() => wake.defineEvent("a.c", { description: "d", args: { n: z.number() }, payload: p, text }), /routes occurrences/);
  assert.throws(() => wake.defineEvent("a.d", { description: "d", args: { y: z.string().optional() }, payload: p, text }), /payload field/);
  wake.defineEvent("a.e", { description: "d", args: {}, payload: p, text });
  assert.throws(() => wake.defineEvent("a.e", { description: "d", args: {}, payload: p, text }), /twice/);
});

test("emit checks the payload, merges the manifest first from a process that serves tools, and sends the rendered line", async () => {
  const { wake, reqs } = fakeCore({ "/events/emit": () => ({ json: { matched: 2, duplicate: false } }) });
  const chat = chatEvent(wake);
  wake.attach(new McpServer({ name: "chat", version: "1.0.0" }));
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada" } as any), (e: unknown) => e instanceof WakeshiftError && e.code === "bad_arguments" && e.detail.startsWith("payload for"));
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada", text: "hi" }, { to: "u1" }), /isn't personal/);
  const out = await chat.emit({ channel: "eng" }, { from: "ada", text: "shipped" }, { id: "m1" });
  assert.deepEqual(out, { matched: 2, duplicate: false, unmatched: [], ended: [] });
  assert.deepEqual(
    reqs.map((r) => `${r.method} ${r.path}`),
    ["POST /manifest/merge", "POST /events/emit"],
  );
  assert.equal(reqs[0].body.events[0].name, "chat.message");
  assert.deepEqual(reqs[1].body, { name: "chat.message", arguments: { channel: "eng" }, payload: { from: "ada", text: "shipped" }, text: "ada: shipped", id: "m1" });
  // Sent once: the next emit goes straight out.
  await chat.emit({ channel: "eng" }, { from: "bob", text: "ok" });
  assert.equal(reqs.filter((r) => r.path.startsWith("/manifest")).length, 1);
});

test("requestApp never sends the manifest from a process with no events, or one that doesn't serve tools", async () => {
  // 0.4.0 sent an empty list here, which cleared every one of the app's events.
  const bare = fakeCore({ "/manifest": () => ({ json: { description: "" } }) });
  await bare.wake.requestApp("GET", "/manifest");
  assert.deepEqual(
    bare.reqs.map((r) => `${r.method} ${r.path}`),
    ["GET /manifest"],
  );
  const { wake, reqs } = fakeCore({ "/events/status": () => ({ json: { subscriptions: 0, connected: 0 } }) });
  const chat = chatEvent(wake);
  await wake.requestApp("GET", "/manifest");
  await chat.status({ channel: "eng" });
  assert.deepEqual(
    reqs.map((r) => `${r.method} ${r.path}`),
    ["GET /manifest", "POST /events/status"],
  );
});

test("a process that only emits never sends the manifest", async () => {
  const { wake, reqs } = fakeCore({ "/events/emit": () => ({ json: { matched: 0 } }) });
  const chat = chatEvent(wake);
  await chat.emit({ channel: "eng" }, { from: "ada", text: "shipped" });
  assert.deepEqual(
    reqs.map((r) => `${r.method} ${r.path}`),
    ["POST /events/emit"],
  );
});

test("a personal event is emitted to one subscriber", async () => {
  const { wake, reqs } = fakeCore({ "/events/emit": () => ({ json: { matched: 1 } }) });
  const turn = turnEvent(wake);
  await assert.rejects(turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }), /personal: pass \{ to \}/);
  await turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "user-abc" });
  assert.equal(reqs.at(-1)!.body.to, "user-abc");
  assert.deepEqual(reqs.at(-1)!.body.payload, { hole: ["As", "Kd"], toCall: 2 });
});

test("a personal event is emitted to many: a plain user alone stays a string, anything else a list", async () => {
  const { wake, reqs } = fakeCore({
    "/events/emit": () => ({
      json: { matched: 1, duplicate: false, unmatched: ["u2", { native: "u3" }], ended: [{ user: "u1", subscription: "es_1" }] },
    }),
  });
  const turn = turnEvent(wake);
  const p = { hole: ["As", "Kd"], toCall: 2 };
  await turn.emit({ table: "juniper" }, p, { to: ["user-abc"] });
  assert.equal(reqs.at(-1)!.body.to, "user-abc");
  await turn.emit({ table: "juniper" }, p, { to: { native: "u3" } });
  assert.deepEqual(reqs.at(-1)!.body.to, [{ native: "u3" }]);
  const out = await turn.emit({ table: "juniper" }, p, { to: [{ user: "u1", subscription: "es_1" }, "u2", { native: "u3" }], id: "h1" });
  assert.deepEqual(reqs.at(-1)!.body.to, [{ user: "u1", subscription: "es_1" }, "u2", { native: "u3" }]);
  assert.equal(reqs.at(-1)!.body.id, "h1");
  assert.deepEqual(out, { matched: 1, duplicate: false, unmatched: ["u2", { native: "u3" }], ended: [{ user: "u1", subscription: "es_1" }] });
  const sent = reqs.length;
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [] }), /personal: pass \{ to \}/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: ["u1", " "] }), /user is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ native: "" }] }), /user is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ user: "u1", subscription: "" }] }), /subscription is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ user: "u1" } as any] }), /a recipient is a user/);
  const chat = chatEvent(wake);
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada", text: "hi" }, { to: [{ native: "u1" }] }), /isn't personal/);
  assert.equal(reqs.length, sent, "nothing refused is sent");
});

test("publishUrl takes exactly one plain user", async () => {
  const { wake, reqs } = fakeCore({ "/events/publish-urls": () => ({ json: { id: "u1", event: "game.turn", url: "http://core.test/x" } }) });
  const turn = turnEvent(wake);
  await turn.publishUrl({ table: "juniper" }, { to: "user-abc" });
  assert.equal(reqs.at(-1)!.body.to, "user-abc");
  const sent = reqs.length;
  await assert.rejects(turn.publishUrl({ table: "juniper" }, { to: ["a", "b"] as any }), /exactly one user/);
  await assert.rejects(turn.publishUrl({ table: "juniper" }, { to: { native: "a" } as any }), /exactly one user/);
  assert.equal(reqs.length, sent);
});

test("status counts by arguments; statusOf reads subscriptions by ID", async () => {
  const { wake, reqs } = fakeCore({
    "/events/status": (body: any) =>
      body.subscriptions
        ? { json: { subscriptions: body.subscriptions.map((s: any) => ({ id: s.id, user: s.user, live: s.id === "s1", connected: s.id === "s1", via: s.id === "s1" ? "stream" : undefined })) } }
        : { json: { subscriptions: 3, connected: 2, via: { stream: 1, poll: 1 } } },
  });
  const turn = turnEvent(wake);
  assert.deepEqual(await turn.status({ table: "juniper" }, { to: " u1 " }), { subscriptions: 3, connected: 2, via: { stream: 1, poll: 1, hooks: 0, webhook: 0 } });
  assert.deepEqual(reqs.at(-1)!.body, { name: "game.turn", arguments: { table: "juniper" }, to: "u1" });
  assert.deepEqual(await turn.statusOf([{ user: "u1", subscription: "s1" }, { user: "u2", subscription: "s2" }]), [
    { id: "s1", user: "u1", live: true, connected: true, via: "stream" },
    { id: "s2", user: "u2", live: false, connected: false },
  ]);
  assert.deepEqual(reqs.at(-1)!.body, { name: "game.turn", subscriptions: [{ user: "u1", id: "s1" }, { user: "u2", id: "s2" }] });
  const sent = reqs.length;
  assert.deepEqual(await turn.statusOf([]), []);
  await assert.rejects(turn.status({ table: "juniper" }), /personal/);
  await assert.rejects(turn.status({ table: "juniper" }, { to: ["a"] as any }), /exactly one user/);
  await assert.rejects(chatEvent(wake).status({ channel: "eng" }, { to: "a" }), /isn't personal/);
  await assert.rejects(turn.statusOf([{ user: "", subscription: "s1" }]), /user and subscription/);
  await assert.rejects(turn.statusOf(Array.from({ length: 1001 }, () => ({ user: "u", subscription: "s" }))), /at most 1000/);
  assert.equal(reqs.length, sent, "nothing refused is sent");
});

test("speaksEvents: the extension, or ChatGPT; not the older top-level key", () => {
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "x"), true);
  assert.equal(speaksEvents({ events: {} }, "x"), false);
  assert.equal(speaksEvents({}, "openai-mcp"), true);
  assert.equal(speaksEvents({}, "claude-code"), false);
});

// connect links a client to server, keeping what the server sends as it
// sent it (the client's own parsing drops capabilities it doesn't know).
const sent = new WeakMap<Client, any[]>();
async function connect(server: McpServer, clientName: string) {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: clientName, version: "1.0.0" });
  const wire: any[] = [];
  const send = a.send.bind(a);
  a.send = async (m, o) => (wire.push(JSON.parse(JSON.stringify(m))), send(m, o));
  sent.set(client, wire);
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

function text(r: unknown): string {
  return ((r as CallToolResult).content[0] as { text: string }).text;
}

test("attach: bridged clients get subscribe_events; every client gets unsubscribe and list", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ json: { event_subscription: { id: "es1", created: true }, addition: { text: "Start the Monitor tool now." } } }),
    "/event-subscriptions/list": () => ({
      json: { subscriptions: [{ id: "es1", event: "chat.message", arguments: { channel: "eng" }, destination: "conversation", created_at: "2026-10-21T10:00:00Z" }] },
    }),
    "/event-subscriptions/end": () => ({ json: { ended: true } }),
    "/event-subscriptions/unsubscribe": () => ({ json: { ended: true } }),
  });
  chatEvent(wake);
  turnEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "claude-code");
  const { tools } = await client.listTools();
  const byName = new Map(tools.map((t) => [t.name, t]));
  assert.deepEqual([...byName.keys()].sort(), ["list_subscriptions", "subscribe_events", "unsubscribe_events"]);
  const sub = byName.get("subscribe_events")!;
  assert.ok(sub.description!.startsWith(SUBSCRIBE_TOOL_DESCRIPTION));
  assert.match(sub.description!, /chat\.message\(channel, from\): A new message in a channel/);
  assert.ok(Object.keys(sub.inputSchema.properties ?? {}).includes("session_key"));
  assert.equal(byName.get("unsubscribe_events")!.description, UNSUBSCRIBE_TOOL_DESCRIPTION);
  assert.equal(byName.get("list_subscriptions")!.description, LIST_TOOL_DESCRIPTION);
  // Only list_subscriptions only reads, and says so.
  assert.equal(byName.get("list_subscriptions")!.annotations?.readOnlyHint, true);
  assert.notEqual(byName.get("subscribe_events")!.annotations?.readOnlyHint, true);
  assert.notEqual(byName.get("unsubscribe_events")!.annotations?.readOnlyHint, true);

  const r = await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" }, harness: "claude-code" } });
  assert.equal(text(r), 'Subscribed to chat.message {"channel":"eng"}.\n\nStart the Monitor tool now.');
  const call = reqs.find((q) => q.path === "/event-subscriptions")!.body;
  assert.equal(call.name, "chat.message");
  assert.deepEqual(call.arguments, { channel: "eng" });
  assert.equal(call.call.user, "u1");
  assert.equal(call.call.client, "claude-code");
  assert.equal(call.call.harness, "claude-code");
  assert.equal(call.call.tool, "subscribe_events", "a connector beacon in the result names the tool");

  // authorize refuses, bad arguments are explained, nothing reaches the core.
  const before = reqs.length;
  const refused = (await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "secret" } } })) as CallToolResult;
  assert.equal(refused.isError, true);
  assert.match(text(refused), /may not subscribe/);
  const bad = (await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { room: "eng" } } })) as CallToolResult;
  assert.equal(bad.isError, true);
  assert.match(text(bad), /arguments for chat\.message/);
  assert.equal(reqs.length, before);

  const listed = await client.callTool({ name: "list_subscriptions", arguments: {} });
  assert.match(text(listed), /- es1: chat\.message \{"channel":"eng"\}, wakes a conversation, since 2026-10-21/);
  assert.match(text(await client.callTool({ name: "unsubscribe_events", arguments: { id: "es1" } })), /Stopped subscription es1/);
  assert.deepEqual(reqs.find((q) => q.path === "/event-subscriptions/end")!.body, { user: "u1", id: "es1" });
  assert.match(
    text(await client.callTool({ name: "unsubscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } })),
    /Stopped chat\.message/,
  );
  await client.close();
});

// The other SDKs echo arguments as Wakeshift keeps them (keys in name
// order), whatever order the event defines them in.
test("attach: the tools echo arguments in name order", async () => {
  const { wake } = fakeCore({
    "/event-subscriptions": () => ({ json: { event_subscription: { id: "es1", created: false }, addition: {} } }),
    "/event-subscriptions/unsubscribe": () => ({ json: { ended: true } }),
  });
  wake.defineEvent("ci.run", {
    description: "A CI run finished.",
    args: z.object({ repo: z.string(), pr: z.number().int(), branch: z.string().optional() }),
    payload: z.object({ status: z.string(), branch: z.string() }),
    text: (r) => r.status,
  });
  const server = new McpServer({ name: "ci", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "claude-code");
  const args = { pr: 5, repo: "web", branch: "main" };
  assert.equal(text(await client.callTool({ name: "subscribe_events", arguments: { event: "ci.run", arguments: args } })), 'Already subscribed to ci.run {"branch":"main","pr":5,"repo":"web"}: renewed.');
  assert.equal(text(await client.callTool({ name: "unsubscribe_events", arguments: { event: "ci.run", arguments: args } })), 'Stopped ci.run {"branch":"main","pr":5,"repo":"web"}.');
  await client.close();
});

test("attach: a personal event needs a known subscriber", async () => {
  const { wake } = fakeCore();
  turnEvent(wake);
  const server = new McpServer({ name: "poker", version: "1.0.0" });
  wake.attach(server, { identify: () => ({}) });
  const client = await connect(server, "claude-code");
  const r = (await client.callTool({ name: "subscribe_events", arguments: { event: "game.turn", arguments: { table: "juniper" } } })) as CallToolResult;
  assert.equal(r.isError, true);
  assert.match(text(r), /personal/);
  const l = (await client.callTool({ name: "list_subscriptions", arguments: {} })) as CallToolResult;
  assert.equal(l.isError, true);
  await client.close();
});

test("attach: without identify, requests have no user", async () => {
  const { wake } = fakeCore();
  turnEvent(wake);
  const server = new McpServer({ name: "poker", version: "1.0.0" });
  wake.attach(server);
  const client = await connect(server, "claude-code");
  const r = (await client.callTool({ name: "subscribe_events", arguments: { event: "game.turn", arguments: { table: "juniper" } } })) as CallToolResult;
  assert.equal(r.isError, true);
  assert.match(text(r), /personal/);
  const l = (await client.callTool({ name: "list_subscriptions", arguments: {} })) as CallToolResult;
  assert.equal(l.isError, true);
  await client.close();
});

test("attach: a client that speaks MCP Events subscribes natively, and isn't offered subscribe_events", async () => {
  const { wake, reqs } = fakeCore({
    "/events/subscribe": () => ({ json: { id: "sub_1", refresh_before: "2026-10-22T10:00:00.000Z", cursor: "c1", truncated: false, created: true } }),
    "/events/unsubscribe": () => ({ json: { ended: false } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "openai-mcp");
  const caps = sent.get(client)!.find((m) => m.result?.capabilities)!.result.capabilities;
  assert.deepEqual(caps.events, { listChanged: false });
  assert.deepEqual(caps.extensions?.[EXTENSION_KEY], { listChanged: false });
  const names = (await client.listTools()).tools.map((t) => t.name).sort();
  assert.deepEqual(names, ["list_subscriptions", "unsubscribe_events"]);

  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.equal(list.resultType, "complete");
  assert.equal(list.events[0].name, "chat.message");
  assert.deepEqual(list.events[0].delivery, ["webhook", "poll"]);

  const delivery = { mode: "webhook", url: "https://chatgpt.example/cb", secret: "whsec_x" };
  const res = (await client.request(
    { method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "eng" }, delivery, ttlMs: 3600000 } } as any,
    z.any() as any,
  )) as any;
  assert.deepEqual(res, { resultType: "complete", id: "sub_1", refreshBefore: "2026-10-22T10:00:00Z", cursor: "c1", truncated: false });
  const body = reqs.find((q) => q.path === "/events/subscribe")!.body;
  assert.equal(body.dialect, "chatgpt");
  assert.equal(body.call.user, "u1");
  assert.equal(body.ttl_ms, 3600000);
  assert.deepEqual(body.delivery, delivery);

  const rpcError = async (params: any): Promise<McpError> => {
    try {
      await client.request({ method: "events/subscribe", params } as any, z.any() as any);
    } catch (e) {
      return e as McpError;
    }
    throw new Error("no error");
  };
  assert.equal((await rpcError({ name: "chat.nope", arguments: {}, delivery })).code, -32011, "ChatGPT's NotFound");
  assert.equal((await rpcError({ name: "chat.message", arguments: { channel: "secret" }, delivery })).code, -32012, "ChatGPT's Forbidden");
  assert.equal((await rpcError({ name: "chat.message", arguments: { room: "x" }, delivery })).code, -32602);
  assert.equal((await rpcError({ name: "chat.message", arguments: { channel: "eng" }, delivery: { mode: "poll" } })).code, -32602, "ChatGPT has no Unsupported");
  // Unsubscribing nothing: ChatGPT gets the empty result it always has.
  const un = (await client.request({ method: "events/unsubscribe", params: { name: "chat.message", arguments: { channel: "eng" }, delivery } } as any, z.any() as any)) as any;
  assert.deepEqual(un, { resultType: "complete" });
  await client.close();
});

test("attach: draft clients get the draft's codes", async () => {
  const { wake } = fakeCore({
    "/events/subscribe": () => ({ status: 400, json: { error: { code: "callback_failed", message: "the callback didn't echo the challenge", reason: "challenge" } } }),
    "/events/unsubscribe": () => ({ json: { ended: false } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "some-draft-client");
  const delivery = { mode: "webhook", url: "https://x.example/cb", secret: "whsec_x" };
  const err = async (method: string, params: any): Promise<McpError> => {
    try {
      await client.request({ method, params } as any, z.any() as any);
    } catch (e) {
      return e as McpError;
    }
    throw new Error("no error");
  };
  const cb = await err("events/subscribe", { name: "chat.message", arguments: { channel: "eng" }, delivery });
  assert.equal(cb.code, -32027);
  assert.deepEqual(cb.data, { reason: "challenge" });
  assert.equal((await err("events/unsubscribe", { name: "chat.message", arguments: { channel: "eng" }, delivery })).code, -32023);
  assert.equal((await err("events/stream", {})).code, -32026);
  assert.equal((await err("events/poll", {})).code, -32023); // no such event
  await client.close();
});

test("attach: a user at the subscription limit gets ResourceExhausted", async () => {
  const message = "this user already has 20 live webhook subscriptions to this app";
  const { wake } = fakeCore({
    "/events/subscribe": () => ({ status: 403, json: { error: { code: "subscription_limit", message } } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "some-draft-client");
  const delivery = { mode: "webhook", url: "https://x.example/cb", secret: "whsec_x" };
  try {
    await client.request({ method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "eng" }, delivery } } as any, z.any() as any);
    assert.fail("no error");
  } catch (e) {
    const err = e as McpError;
    assert.equal(err.code, -32025);
    assert.match(err.message, /20 live webhook subscriptions/);
    assert.deepEqual(err.data, { limit: "subscriptions" });
  }
  await client.close();
});

// The constants match Wakeshift's own (internal/mcpevents/event/spec.go),
// where the repo has it (the published SDK doesn't).
test("spec constants match the core's", async () => {
  const { existsSync, readFileSync } = await import("node:fs");
  const { fileURLToPath } = await import("node:url");
  const goFile = fileURLToPath(new URL("../../../../internal/mcpevents/event/spec.go", import.meta.url));
  if (!existsSync(goFile)) return;
  const go = readFileSync(goFile, "utf8");
  const spec = await import("../src/spec.js");
  const str = (name: string) => go.match(new RegExp(`${name}\\s*=\\s*"([^"]+)"`))?.[1];
  assert.equal(spec.REVISION, str("Revision"));
  assert.equal(spec.EXTENSION_KEY, str("ExtensionKey"));
  assert.equal(spec.TOP_LEVEL_KEY, str("TopLevelKey"));
  assert.equal(spec.RESULT_TYPE, str("ResultType"));
  assert.equal(spec.TEXT_META, str("TextMeta"));
  assert.equal(spec.NAME_HEADER, str("NameHeader")?.toLowerCase());
  const draft = go.match(/return Codes\{NotFound: (-\d+), Forbidden: (-\d+), ResourceExhausted: (-\d+), Unsupported: (-\d+), CallbackEndpoint: (-\d+)\}/)!;
  assert.deepEqual(Object.values(spec.codesFor("draft")), draft.slice(1).map(Number));
  const chatgpt = go.match(/return Codes\{NotFound: (-\d+), Forbidden: (-\d+), ResourceExhausted: InvalidParams, Unsupported: InvalidParams, CallbackEndpoint: (-\d+)\}/)!;
  const c = spec.codesFor("chatgpt");
  assert.deepEqual([c.notFound, c.forbidden, c.callbackEndpoint], chatgpt.slice(1).map(Number));
  assert.equal(c.unsupported, spec.INVALID_PARAMS);
});

test("an authorize that throws refuses, on both paths, and the app hears why", async () => {
  const warnings: string[] = [];
  const reqs: string[] = [];
  const fakeFetch = (async (url: string | URL | Request) => {
    reqs.push(String(url));
    return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: fakeFetch, onWarning: (m) => warnings.push(m) });
  wake.defineEvent("chat.message", {
    description: "A message",
    args: { channel: z.string() },
    payload: z.object({ text: z.string() }),
    text: (m) => m.text,
    authorize: () => {
      throw new Error("the ACL service is down");
    },
  });
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const bridged = await connect(server, "claude-code");
  const r = (await bridged.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } })) as CallToolResult;
  assert.equal(r.isError, true);
  assert.match(text(r), /may not subscribe/);
  await bridged.close();
  // The same server, reconnected to a native client: it's no longer offered subscribe_events, and is refused with Forbidden.
  const native = await connect(server, "openai-mcp");
  assert.deepEqual((await native.listTools()).tools.map((t) => t.name).sort(), ["list_subscriptions", "unsubscribe_events"]);
  await assert.rejects(
    native.request(
      { method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "eng" }, delivery: { mode: "webhook", url: "https://x.example/cb", secret: "whsec_x" } } } as any,
      z.any() as any,
    ),
    (e: unknown) => e instanceof McpError && e.code === -32012,
  );
  await native.close();
  // And back to a bridged client: offered again.
  const again = await connect(server, "claude-code");
  assert.ok((await again.listTools()).tools.some((t) => t.name === "subscribe_events"));
  await again.close();
  assert.equal(warnings.length, 2);
  assert.match(warnings[0], /authorize for chat\.message threw.*ACL service is down/);
  assert.equal(reqs.filter((u) => u.includes("subscribe")).length, 0, "nothing reached the core");
});

test("attach's tools option picks which of its own tools it adds: all three by default, the ones named, or none", async () => {
  assert.deepEqual([...BUILTIN_TOOLS], ["subscribe_events", "unsubscribe_events", "list_subscriptions"]);
  const listed = async (tools?: readonly BuiltinTool[]) => {
    const { wake } = fakeCore();
    chatEvent(wake);
    const server = new McpServer({ name: "chat", version: "1.0.0" });
    server.registerTool("ping", { inputSchema: {} }, async () => ({ content: [] }));
    wake.attach(server, { identify: () => ({ user: "u1" }), tools });
    const client = await connect(server, "claude-code");
    return (await client.listTools()).tools.map((t) => t.name).sort();
  };
  assert.deepEqual(await listed(), ["list_subscriptions", "ping", "subscribe_events", "unsubscribe_events"]);
  assert.deepEqual(await listed(["list_subscriptions", "unsubscribe_events"]), ["list_subscriptions", "ping", "unsubscribe_events"]);
  assert.deepEqual(await listed([]), ["ping"]);
  const { wake } = fakeCore();
  chatEvent(wake);
  assert.throws(() => wake.attach(new McpServer({ name: "x", version: "1.0.0" }), { tools: ["subscribe" as BuiltinTool] }), /"subscribe"/);
});

test("attach needs events, and must come before connecting", async () => {
  const { wake } = fakeCore();
  const server = new McpServer({ name: "x", version: "1.0.0" });
  assert.throws(() => wake.attach(server), /at least one event/);
  chatEvent(wake);
  const late = new McpServer({ name: "y", version: "1.0.0" });
  late.registerTool("ping", { inputSchema: {} }, async () => ({ content: [] }));
  const [a] = InMemoryTransport.createLinkedPair();
  await late.connect(a);
  assert.throws(() => wake.attach(late), /before connecting/);
});

test("an event's end: ends one of a user's subscriptions by ID, outside a tool", async () => {
  const { wake, reqs } = fakeCore({ "/event-subscriptions/end": (b) => ({ json: { ended: b.id === "es_1" } }) });
  const turn = turnEvent(wake);
  assert.equal(await turn.end("u1", "es_1"), true);
  assert.equal(await turn.end("u1", "es_other"), false);
  assert.deepEqual(
    reqs.filter((r) => r.path === "/event-subscriptions/end").map((r) => r.body),
    [
      { user: "u1", id: "es_1" },
      { user: "u1", id: "es_other" },
    ],
  );
});

// The Go SDK (pkg/appkit) writes test/event-tools.json and
// test/events-list.json (WAKESHIFT_UPDATE_GOLDEN=1 go test ./pkg/appkit)
// and tests against them too, so the two SDKs can't drift.
function goldenFile(name: string): any {
  return JSON.parse(readFileSync(new URL(`../../test/${name}`, import.meta.url), "utf8"));
}

// sorted is v with every object's keys in order, as Go writes JSON.
function sorted(v: any): any {
  if (Array.isArray(v)) return v.map(sorted);
  if (v && typeof v === "object") return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sorted(v[k])]));
  return v;
}

// inputShape is what the golden file keeps of a tool's input schema: each
// property's type, description and enum, and what's required (the rest is
// each MCP SDK's own rendering; the Go test does the same).
function inputShape(s: any): any {
  const properties = Object.fromEntries(
    Object.entries((s.properties ?? {}) as Record<string, any>).map(([k, p]) => [k, { type: p.type, description: p.description, ...(p.enum ? { enum: p.enum } : {}) }]),
  );
  return { properties, ...(s.required ? { required: s.required } : {}) };
}

test("the event tools' wording is the Go SDK's (test/event-tools.json)", () => {
  assert.deepEqual(sorted({ ...EVENT_TOOL_TEXT }), goldenFile("event-tools.json"));
});

test("events/list, tools/list and the manifest match the Go SDK's for the same events (test/events-list.json)", async () => {
  const { wake } = fakeCore();
  const chat = wake.defineEvent("chat.message", {
    title: "Chat message",
    description: "A new message in a channel.",
    args: z.object({ channel: z.string().describe("The channel, lowercase"), from: z.string().optional().describe("Only messages from this person") }),
    payload: z.object({ from: z.string().describe("Who wrote it"), text: z.string().describe("The message") }),
    text: (m) => `${m.from}: ${m.text}`,
  });
  const turn = wake.defineEvent("game.turn", {
    description: "It's your move at a table.",
    args: z.object({ table: z.string().describe("The table you're seated at") }),
    payload: z.object({ street: z.string().describe("The betting round"), hand: z.array(z.string()).optional().describe("Your hole cards") }),
    text: (_t, a) => `Your move at ${a.table}`,
    personal: true,
  });
  // Defined out of alphabetical order: arguments and required fields are
  // listed in name order, as the other SDKs list them.
  const run = wake.defineEvent("ci.run", {
    description: "A CI run on a pull request finished.",
    args: z.object({
      repo: z.string().describe("The repository"),
      pr: z.number().int().describe("The pull request"),
      branch: z.string().optional().describe("Only runs on this branch"),
    }),
    payload: z.object({ status: z.string().describe("How it ended"), branch: z.string().describe("The branch it ran on") }),
    text: (r) => `CI ${r.status}`,
  });
  const server = new McpServer({ name: "sample", version: "1" });
  wake.attach(server, {});
  const client = await connect(server, "claude-code");
  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  const { tools } = await client.listTools();
  const got = {
    events: list.events,
    manifest: [chat.manifest, turn.manifest, run.manifest],
    tools: tools
      .map((t) => ({ name: t.name, description: t.description, inputSchema: inputShape(t.inputSchema), ...(t.annotations ? { annotations: t.annotations } : {}) }))
      .sort((a, b) => (a.name < b.name ? -1 : 1)),
  };
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(got))), goldenFile("events-list.json"));
});

// test/mcp-events-wire.json: the wire format every SDK serves to a draft
// client, both sides of each events/subscribe and events/poll.
test("the MCP Events wire format is every SDK's (test/mcp-events-wire.json)", async () => {
  const g = goldenFile("mcp-events-wire.json");
  const spec = await import("../src/spec.js");
  assert.equal(g.revision, spec.REVISION);
  assert.deepEqual(g.delivery, [...spec.DELIVERY]);
  for (const kind of ["subscribe", "poll"] as const) {
    for (const c of g[kind]) {
      const { wake, reqs } = fakeCore({ [`/events/${kind}`]: () => ({ json: c.core_response }) });
      chatEvent(wake);
      const server = new McpServer({ name: "chat", version: "1.0.0" });
      wake.attach(server, { identify: () => ({ user: "u1" }) });
      const client = await connect(server, "some-draft-client");
      const res = await client.request({ method: `events/${kind}`, params: c.params } as any, z.any() as any);
      assert.deepEqual(res, c.result, `${kind}: ${c.name}: result`);
      const { call, dialect: _dialect, ...body } = reqs.find((q) => q.path === `/events/${kind}`)!.body;
      assert.equal(call.user, "u1", `${kind}: ${c.name}: the caller`);
      assert.deepEqual(body, c.core_request, `${kind}: ${c.name}: what Wakeshift is asked`);
      await client.close();
    }
  }
});

// rawRpc sends JSON-RPC requests straight to server, with no initialize
// first (protocol 2026-07-28 has none), returning each response.
async function rawRpc(server: McpServer) {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const waiting = new Map<number, (m: any) => void>();
  b.onmessage = (m: any) => waiting.get(m.id)?.(m);
  await server.connect(a);
  await b.start();
  let n = 0;
  return (method: string, params: any = {}) =>
    new Promise<any>((resolve) => {
      const id = ++n;
      waiting.set(id, resolve);
      b.send({ jsonrpc: "2.0", id, method, params } as any);
    });
}

const as2026 = (caps: any = {}) => ({
  _meta: {
    "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientInfo": { name: "openclaw", version: "2026.10.10" },
    "io.modelcontextprotocol/clientCapabilities": caps,
  },
});

// Protocol 2026-07-28 on the MCP SDK 1.x (discover.ts): server/discover,
// resultType on every 2026-07-28 result (and only those), the version the
// transport accepts, and no subscribe_events for a request that declares the
// extension itself.
test("attach: a stateless server answers protocol 2026-07-28 clients", async () => {
  const { wake } = fakeCore();
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" }, { instructions: "A chat app." });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  server.registerTool("ping", { description: "Ping" }, async () => ({ content: [{ type: "text", text: "pong" }] }));
  const rpc = await rawRpc(server);
  const { SUPPORTED_PROTOCOL_VERSIONS } = await import("@modelcontextprotocol/sdk/types.js");
  assert.ok(SUPPORTED_PROTOCOL_VERSIONS.includes("2026-07-28"), "the transport takes the version's header");

  const d = (await rpc("server/discover", as2026())).result;
  assert.equal(d.resultType, "complete");
  assert.equal(d.supportedVersions[0], "2026-07-28");
  assert.ok(d.supportedVersions.includes("2025-11-25"));
  assert.deepEqual(d.capabilities.events, { listChanged: false });
  assert.deepEqual(d.capabilities.extensions[EXTENSION_KEY], { listChanged: false });
  assert.ok(d.capabilities.tools);
  assert.deepEqual(d._meta["io.modelcontextprotocol/serverInfo"], { name: "chat", version: "1.0.0" });
  assert.equal(d.instructions, "A chat app.");

  const tools = (await rpc("tools/list", as2026())).result;
  assert.equal(tools.resultType, "complete");
  assert.ok(tools.tools.some((t: any) => t.name === "subscribe_events"), "a client that doesn't declare the extension is offered it");
  const nativeTools = (await rpc("tools/list", as2026({ extensions: { [EXTENSION_KEY]: {} } }))).result;
  assert.deepEqual(nativeTools.tools.map((t: any) => t.name).sort(), ["list_subscriptions", "ping", "unsubscribe_events"]);
  const call = (await rpc("tools/call", { name: "ping", arguments: {}, ...as2026() })).result;
  assert.equal(call.resultType, "complete");
  assert.equal(call.content[0].text, "pong");
  const list = (await rpc("events/list", as2026())).result;
  assert.equal(list.resultType, "complete");

  // An older client's results are as they were.
  const old = (await rpc("tools/list", {})).result;
  assert.equal(old.resultType, undefined);
});
