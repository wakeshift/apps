import { test } from "node:test";
import assert from "node:assert/strict";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { McpError, type CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { readFileSync } from "node:fs";
import {
  EVENT_TOOL_TEXT,
  EXTENSION_KEY,
  LIST_TOOL_DESCRIPTION,
  SUBSCRIBE_TOOL_DESCRIPTION,
  UNSUBSCRIBE_TOOL_DESCRIPTION,
  Wakeshift,
  WakeshiftError,
  speaksEvents,
} from "../src/index.js";

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
  assert.deepEqual(turn.definition().delivery, ["webhook"]);
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

test("emit checks the payload, publishes the manifest first, and sends the rendered line", async () => {
  const { wake, reqs } = fakeCore({ "/events/emit": () => ({ json: { matched: 2, duplicate: false } }) });
  const chat = chatEvent(wake);
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada" } as any), (e: unknown) => e instanceof WakeshiftError && e.code === "bad_payload");
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada", text: "hi" }, { to: "u1" }), /isn't personal/);
  const out = await chat.emit({ channel: "eng" }, { from: "ada", text: "shipped" }, { id: "m1" });
  assert.deepEqual(out, { matched: 2, duplicate: false });
  assert.deepEqual(
    reqs.map((r) => `${r.method} ${r.path}`),
    ["PUT /manifest", "POST /events/emit"],
  );
  assert.equal(reqs[0].body.events[0].name, "chat.message");
  assert.deepEqual(reqs[1].body, { name: "chat.message", arguments: { channel: "eng" }, payload: { from: "ada", text: "shipped" }, text: "ada: shipped", id: "m1" });
  // Published once: the next emit goes straight out.
  await chat.emit({ channel: "eng" }, { from: "bob", text: "ok" });
  assert.equal(reqs.filter((r) => r.path === "/manifest").length, 1);
});

test("a personal event is emitted to one subscriber", async () => {
  const { wake, reqs } = fakeCore({ "/events/emit": () => ({ json: { matched: 1 } }) });
  const turn = turnEvent(wake);
  await assert.rejects(turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }), /personal: pass \{ to \}/);
  await turn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "guest:abc" });
  assert.equal(reqs.at(-1)!.body.to, "guest:abc");
  assert.deepEqual(reqs.at(-1)!.body.payload, { hole: ["As", "Kd"], toCall: 2 });
});

test("speaksEvents: the extension, the older key, or ChatGPT", () => {
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "x"), true);
  assert.equal(speaksEvents({ events: {} }, "x"), true);
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

test("serveEvents: bridged clients get subscribe_events; every client gets unsubscribe and list", async () => {
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
  wake.serveEvents(server, { user: () => "u1" });
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

  const r = await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" }, harness: "claude-code" } });
  assert.equal(text(r), 'Subscribed to chat.message {"channel":"eng"}.\n\nStart the Monitor tool now.');
  const call = reqs.find((q) => q.path === "/event-subscriptions")!.body;
  assert.equal(call.name, "chat.message");
  assert.deepEqual(call.arguments, { channel: "eng" });
  assert.equal(call.call.user, "u1");
  assert.equal(call.call.client, "claude-code");
  assert.equal(call.call.harness, "claude-code");

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

test("serveEvents: a personal event needs a known subscriber", async () => {
  const { wake } = fakeCore();
  turnEvent(wake);
  const server = new McpServer({ name: "poker", version: "1.0.0" });
  wake.serveEvents(server, { user: () => undefined });
  const client = await connect(server, "claude-code");
  const r = (await client.callTool({ name: "subscribe_events", arguments: { event: "game.turn", arguments: { table: "juniper" } } })) as CallToolResult;
  assert.equal(r.isError, true);
  assert.match(text(r), /personal/);
  const l = (await client.callTool({ name: "list_subscriptions", arguments: {} })) as CallToolResult;
  assert.equal(l.isError, true);
  await client.close();
});

test("serveEvents: a client that speaks MCP Events subscribes natively, and isn't offered subscribe_events", async () => {
  const { wake, reqs } = fakeCore({
    "/events/subscribe": () => ({ json: { id: "sub_1", refresh_before: "2026-10-22T10:00:00.000Z", cursor: "c1", truncated: false, created: true } }),
    "/events/unsubscribe": () => ({ json: { ended: false } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.serveEvents(server, { user: () => "u1" });
  const client = await connect(server, "openai-mcp");
  const caps = sent.get(client)!.find((m) => m.result?.capabilities)!.result.capabilities;
  assert.deepEqual(caps.events, { listChanged: false });
  assert.deepEqual(caps.extensions?.[EXTENSION_KEY], { listChanged: false });
  const names = (await client.listTools()).tools.map((t) => t.name).sort();
  assert.deepEqual(names, ["list_subscriptions", "unsubscribe_events"]);

  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.equal(list.resultType, "complete");
  assert.equal(list.events[0].name, "chat.message");
  assert.deepEqual(list.events[0].delivery, ["webhook"]);

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

test("serveEvents: draft clients get the draft's codes", async () => {
  const { wake } = fakeCore({
    "/events/subscribe": () => ({ status: 400, json: { error: { code: "callback_failed", message: "the callback didn't echo the challenge", reason: "challenge" } } }),
    "/events/unsubscribe": () => ({ json: { ended: false } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.serveEvents(server, { user: () => "u1" });
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
  assert.equal((await err("events/poll", {})).code, -32026);
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
  assert.equal(spec.LEGACY_KEY, str("LegacyKey"));
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
  wake.serveEvents(server, { user: () => "u1" });
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

test("serveEvents needs events, and must come before connecting", async () => {
  const { wake } = fakeCore();
  const server = new McpServer({ name: "x", version: "1.0.0" });
  assert.throws(() => wake.serveEvents(server), /at least one event/);
  chatEvent(wake);
  const late = new McpServer({ name: "y", version: "1.0.0" });
  late.registerTool("ping", { inputSchema: {} }, async () => ({ content: [] }));
  const [a] = InMemoryTransport.createLinkedPair();
  await late.connect(a);
  assert.throws(() => wake.serveEvents(late), /before connecting/);
});

test("an event's subscribe and end: a tool subscribes its own caller, and ends a subscription by ID", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({
      json: {
        event_subscription: { id: "es_1", event: "game.turn", arguments: { table: "t1" }, created: true },
        session: "wsc_abc",
        addition: { text: "Watch with the Monitor tool." },
        agent: { harness: "claude-code", via: "client", claimed: null },
      },
    }),
    "/event-subscriptions/end": (b) => ({ json: { ended: b.id === "es_1" } }),
  });
  const turn = turnEvent(wake);
  // Personal: the subscription belongs to a user, so one is required.
  await assert.rejects(turn.subscribe({} as any, { table: "t1" }, { client: "claude-code" }), /personal: pass user/);
  await assert.rejects(turn.subscribe({} as any, {} as any, { client: "claude-code", user: "u1" }), /arguments for game\.turn/);
  const sub = await turn.subscribe({} as any, { table: "t1" }, { client: "claude-code", user: "u1", wakeArgs: { session_key: "wsc_abc" } });
  assert.equal(sub.id, "es_1");
  assert.equal(sub.created, true);
  assert.equal(sub.session, "wsc_abc");
  assert.equal(sub.agent.harness, "claude-code");
  const res = sub.result("Seated.");
  assert.match(JSON.stringify(res), /Seated\./);
  assert.match(JSON.stringify(res), /Watch with the Monitor tool/);
  const req = reqs.find((r) => r.path === "/event-subscriptions")!;
  assert.equal(req.body.name, "game.turn");
  assert.deepEqual(req.body.arguments, { table: "t1" });
  assert.equal(req.body.call.user, "u1");
  assert.equal(req.body.call.session, "wsc_abc");
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

test("an event's unsubscribe: a tool ends its own caller's subscription with those arguments", async () => {
  const { wake, reqs } = fakeCore({ "/event-subscriptions/unsubscribe": (b) => ({ json: { ended: b.arguments.table === "t1" } }) });
  const turn = turnEvent(wake);
  assert.equal(await turn.unsubscribe({} as any, { table: "t1" }, { client: "claude-code", user: "u1", wakeArgs: { session_key: "wsc_abc" } }), true);
  assert.equal(await turn.unsubscribe({} as any, { table: "t2" }, { client: "claude-code", user: "u1" }), false);
  await assert.rejects(turn.unsubscribe({} as any, {} as any, { client: "claude-code" }), /arguments for game\.turn/);
  const req = reqs.find((r) => r.path === "/event-subscriptions/unsubscribe")!;
  assert.equal(req.body.name, "game.turn");
  assert.deepEqual(req.body.arguments, { table: "t1" });
  assert.equal(req.body.call.session, "wsc_abc");
  assert.equal(req.body.call.user, "u1");
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
  const server = new McpServer({ name: "sample", version: "1" });
  wake.serveEvents(server, {});
  const client = await connect(server, "claude-code");
  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  const { tools } = await client.listTools();
  const got = {
    events: list.events,
    manifest: [chat.manifest, turn.manifest],
    tools: tools
      .map((t) => ({ name: t.name, description: t.description, inputSchema: inputShape(t.inputSchema) }))
      .sort((a, b) => (a.name < b.name ? -1 : 1)),
  };
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(got))), goldenFile("events-list.json"));
});
