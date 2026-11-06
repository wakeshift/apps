import { test } from "node:test";
import assert from "node:assert/strict";
import { MCP, McpServer, rawRpc } from "./mcp.js";
import { Client } from "./mcp.js";
import { InMemoryTransport } from "./mcp.js";
import { McpError, type CallToolResult } from "./mcp.js";
import { z } from "zod";
import { readFileSync } from "node:fs";
import { Wakeshift, UsageError, type AttachOptions } from "../src/index.js";
import { EVENT_TOOL_TEXT } from "../src/events.js";
import { EXTENSION_KEY, dialectOf } from "../src/spec.js";
import { OUTPUT_SCHEMAS as TOOL_OUTPUT_SCHEMAS } from "../src/wording.js";

const BUILTIN_TOOLS = ["subscribe_events", "unsubscribe_events", "list_subscriptions"] as const;
type BuiltinTool = (typeof BUILTIN_TOOLS)[number];
const SUBSCRIBE_TOOL_DESCRIPTION = EVENT_TOOL_TEXT.subscribe_description;
const UNSUBSCRIBE_TOOL_DESCRIPTION = EVENT_TOOL_TEXT.unsubscribe_description;
const LIST_TOOL_DESCRIPTION = EVENT_TOOL_TEXT.list_description;
import { quiet, warningsInto } from "./logs.js";
import { WAKE_ARGS_JSON, withWakeArgsJson } from "../src/toolcall.js";
import * as z3 from "zod/v3";
import { speaksEvents, toolChoice } from "../src/events.js";
import { nativeOf } from "../src/toolcall.js";
import { TOOL_ANNOTATIONS } from "../src/tasks.js";

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
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ method: init?.method ?? "GET", path, body });
    const r = replies[path]?.(body) ?? { json: {} };
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const warnings: string[] = [];
  return { reqs, warnings, wake: new Wakeshift({ key: KEY, core: "https://core.test", fetch: fakeFetch, logger: warningsInto(warnings) }) };
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
  await assert.rejects(chat.emit({ channel: "eng" }, { from: "ada" } as any), (e: unknown) => e instanceof UsageError && e.code === "bad_arguments" && e.message.replace("wakeshift: ", "").startsWith("payload for"));
  await assert.rejects((chat.emit as any)({ channel: "eng" }, { from: "ada", text: "hi" }, { to: "u1" }), /isn't personal/);
  const out = await chat.emit({ channel: "eng" }, { from: "ada", text: "shipped" }, { id: "m1" });
  assert.deepEqual(out, { matched: 2, duplicate: false, unmatched: [], ended: [], textTrimmed: false });
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

test("a process that only emits sends no manifest while Wakeshift knows the event", async () => {
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
  await assert.rejects((turn.emit as any)({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }), /personal: pass \{ to \}/);
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
  assert.deepEqual(out, { matched: 1, duplicate: false, unmatched: ["u2", { native: "u3" }], ended: [{ user: "u1", subscription: "es_1" }], textTrimmed: false });
  const sent = reqs.length;
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [] }), /personal: pass \{ to \}/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: ["u1", " "] }), /user is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ native: "" }] }), /user is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ user: "u1", subscription: "" }] }), /subscription is empty/);
  await assert.rejects(turn.emit({ table: "juniper" }, p, { to: [{ user: "u1" } as any] }), /a recipient is a user/);
  const chat = chatEvent(wake);
  await assert.rejects((chat.emit as any)({ channel: "eng" }, { from: "ada", text: "hi" }, { to: [{ native: "u1" }] }), /isn't personal/);
  assert.equal(reqs.length, sent, "nothing refused is sent");
});

test("publishUrl takes exactly one plain user", async () => {
  const { wake, reqs } = fakeCore({ "/events/publish-urls": () => ({ json: { id: "u1", event: "game.turn", url: "https://core.test/x" } }) });
  const turn = turnEvent(wake);
  await turn.createPublishUrl({ table: "juniper" }, { to: "user-abc" });
  assert.equal(reqs.at(-1)!.body.to, "user-abc");
  const sent = reqs.length;
  await assert.rejects(turn.createPublishUrl({ table: "juniper" }, { to: ["a", "b"] as any }), /exactly one user/);
  await assert.rejects(turn.createPublishUrl({ table: "juniper" }, { to: { native: "a" } as any }), /exactly one user/);
  assert.equal(reqs.length, sent);
});

test("publishUrl passes a signing secret, and reads the fingerprint back, never the secret", async () => {
  const { wake, reqs } = fakeCore({
    "/events/publish-urls": (b) => ({
      json: { id: "pu1", event: "chat.message", url: "https://core.test/x", ...(b.signing_secret ? { signing: { fingerprint: "abc123def456", set_at: "2026-11-04T00:00:00Z" } } : {}) },
    }),
  });
  const chat = chatEvent(wake);
  const secret = "whsec_" + Buffer.alloc(32, 1).toString("base64");
  const url = await chat.createPublishUrl({ channel: "eng" }, { signingSecret: secret });
  assert.equal(reqs.at(-1)!.body.signing_secret, secret);
  assert.deepEqual(url, { id: "pu1", event: "chat.message", url: "https://core.test/x", signing: { fingerprint: "abc123def456", setAt: "2026-11-04T00:00:00Z" } });
  assert.ok(!JSON.stringify(url).includes(secret));
  const plain = await chat.createPublishUrl({ channel: "eng" });
  assert.equal(reqs.at(-1)!.body.signing_secret, undefined);
  assert.equal(plain.signing, undefined);
  const sent = reqs.length;
  await assert.rejects(chat.createPublishUrl({ channel: "eng" }, { signingSecret: " " }), (err: UsageError) => err.code === "bad_signing_secret" && !err.message.includes("whsec_A"));
  assert.equal(reqs.length, sent);
});

test("a zod 3 payload takes fields a sender adds: no additionalProperties: false unless the object is strict", () => {
  const { wake } = fakeCore();
  const ev = wake.defineEvent("hook.done", {
    description: "A webhook arrived",
    args: z3.object({ job: z3.string() }),
    payload: z3.object({
      id: z3.string(),
      data: z3.object({ n: z3.number() }).optional(),
      items: z3.array(z3.object({ k: z3.string() })),
      either: z3.union([z3.object({ a: z3.string() }), z3.object({ b: z3.string() }).strict()]),
      strict: z3.object({ s: z3.string() }).strict(),
      nullable: z3.object({ x: z3.string() }).nullable(),
    }),
    text: (p) => p.id,
  });
  const p = ev.manifest.payload as any;
  assert.equal(p.additionalProperties, undefined);
  assert.equal(p.properties.data.additionalProperties, undefined);
  assert.equal(p.properties.items.items.additionalProperties, undefined);
  assert.equal(p.properties.either.anyOf[0].additionalProperties, undefined);
  assert.equal(p.properties.either.anyOf[1].additionalProperties, false, "a strict object still refuses");
  assert.equal(p.properties.strict.additionalProperties, false);
  assert.ok(!JSON.stringify(p.properties.nullable).includes('"additionalProperties":false'), JSON.stringify(p.properties.nullable));
  // Arguments aren't a sender's: they render as before.
  assert.equal((ev.manifest.arguments as any).additionalProperties, false);
});

test("Wakeshift's arguments as tools/list adds them are every SDK's (test/wake-args.json), added to a copy", () => {
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(WAKE_ARGS_JSON))), sorted(goldenFile("wake-args.json").compact.subscribing.properties));
  const mine = { type: "object", properties: { job: { type: "string" } }, required: ["job"], additionalProperties: false };
  const got = withWakeArgsJson(mine);
  assert.deepEqual(got, { ...mine, properties: { ...WAKE_ARGS_JSON, job: { type: "string" } } });
  assert.deepEqual(mine.properties, { job: { type: "string" } }, "the schema given stays as it was");
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
  await assert.rejects((turn.status as any)({ table: "juniper" }), /personal/);
  await assert.rejects(turn.status({ table: "juniper" }, { to: ["a"] as any }), /exactly one user/);
  await assert.rejects((chatEvent(wake).status as any)({ channel: "eng" }, { to: "a" }), /isn't personal/);
  await assert.rejects(turn.statusOf([{ user: "", subscription: "s1" }]), /user and subscription/);
  await assert.rejects(turn.statusOf(Array.from({ length: 1001 }, () => ({ user: "u", subscription: "s" }))), /at most 1000/);
  assert.equal(reqs.length, sent, "nothing refused is sent");
});

test("dialectOf: ChatGPT by name, or by User-Agent, through Codex too", () => {
  // Work chats send tools/call and events/* as "openai-mcp/1.0.0 (Codex)", events/list as "openai-mcp/1.0.0".
  assert.equal(dialectOf("", "openai-mcp/1.0.0 (Codex)"), "chatgpt");
  assert.equal(dialectOf("", "openai-mcp/1.0.0"), "chatgpt");
  assert.equal(dialectOf("openai-mcp", ""), "chatgpt");
  assert.equal(dialectOf("", "codex-mcp-client/0.166.0"), "draft");
});

test("speaksEvents: the extension, or ChatGPT; not the older top-level key", () => {
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "x"), true);
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "claude-code"), false, "a harness Wakeshift wakes itself: its name wins");
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "Cursor"), false);
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "capy"), false, "Capy, woken by its webhook watch or a command");
  assert.equal(speaksEvents({ extensions: { [EXTENSION_KEY]: {} } }, "muse-custom-connector"), false, "Muse, woken by a one-shot command");
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

// wireResult is the last result the server sent the client, as it sent it.
function wireResult(client: Client): any {
  return sent.get(client)!.filter((m) => m && "result" in m).at(-1)?.result;
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
    "/event-subscriptions/unsubscribe": () => ({ json: { ended: true, addition: { text: "Keep passing session_key." } } }),
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
  // Each with every SDK's annotations (test/tool-annotations.json): only
  // list_subscriptions only reads, and says so.
  const annotations = goldenFile("tool-annotations.json");
  for (const [name, t] of byName) assert.deepEqual(t.annotations, annotations[name], name);
  assert.equal(byName.get("list_subscriptions")!.annotations?.readOnlyHint, true);

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
  // Wakeshift's addition (the session key stays the same) follows the reply.
  assert.equal(
    text(await client.callTool({ name: "unsubscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } })),
    'Stopped chat.message {"channel":"eng"}.\n\nKeep passing session_key.',
  );
  await client.close();
});

test("attach's own tools have every SDK's output schemas (test/tool-output-schemas.json), and answer with matching structuredContent", async () => {
  const schemas = Object.fromEntries(Object.entries(goldenFile("tool-output-schemas.json")).filter(([k]) => !k.startsWith("_")));
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(TOOL_OUTPUT_SCHEMAS))), sorted(schemas));
  const { wake } = fakeCore({
    "/event-subscriptions": () => ({ json: { event_subscription: { id: "es1", created: true }, addition: { text: "Start the Monitor tool now.", meta: { "wakeshift/connector": "tok" } } } }),
    "/event-subscriptions/list": () => ({
      json: { subscriptions: [{ id: "es1", event: "chat.message", arguments: { channel: "eng" }, destination: "conversation", created_at: "2026-10-21T10:00:00.123456Z" }] },
    }),
    "/event-subscriptions/end": () => ({ json: { ended: false } }),
    "/event-subscriptions/unsubscribe": () => ({ json: { ended: true } }),
  });
  chatEvent(wake);
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  // The MCP SDK's client checks each result's structuredContent against the tool's listed output schema.
  const client = await connect(server, "claude-code");
  const { tools } = await client.listTools();
  for (const t of tools) assert.deepEqual(t.outputSchema, schemas[t.name], t.name);
  const sub = (await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } })) as CallToolResult;
  assert.deepEqual(sub.structuredContent, { subscribed: { event: "chat.message", arguments: { channel: "eng" }, created: true }, wakeshift: "Start the Monitor tool now." });
  assert.equal(text(sub), 'Subscribed to chat.message {"channel":"eng"}.\n\nStart the Monitor tool now.');
  assert.deepEqual(sub._meta, { "wakeshift/connector": "tok" });
  const listed = (await client.callTool({ name: "list_subscriptions", arguments: {} })) as CallToolResult;
  assert.deepEqual(listed.structuredContent, {
    subscriptions: [{ id: "es1", event: "chat.message", arguments: { channel: "eng" }, destination: "conversation", since: "2026-10-21T10:00:00Z" }],
  });
  assert.deepEqual(((await client.callTool({ name: "unsubscribe_events", arguments: { id: "es9" } })) as CallToolResult).structuredContent, { ended: false });
  assert.deepEqual(
    ((await client.callTool({ name: "unsubscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } })) as CallToolResult).structuredContent,
    { ended: true },
  );
  const refused = (await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "secret" } } })) as CallToolResult;
  assert.equal(refused.isError, true);
  assert.equal(refused.structuredContent, undefined);
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
    public: true,
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
  await client.close();
});

test("list_subscriptions and unsubscribe_events by id, with no user: the calling conversation's, by its session_key", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions/list": () => ({
      json: { subscriptions: [{ id: "es1", event: "crawl.finished", arguments: { job: "j1" }, destination: "conversation", created_at: "2026-10-21T10:00:00Z" }] },
    }),
    "/event-subscriptions/end": () => ({ json: { ended: true } }),
  });
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const server = new McpServer({ name: "crawler", version: "1.0.0" });
  const attached = wake.attach(server);
  attached.registerTool("start_crawl", { description: "Crawl.", inputSchema: z.object({ url: z.string() }), subscribes: [crawl] }, async () => "ok");
  const client = await connect(server, "claude-code");
  const listedTool = (await client.listTools()).tools.find((t) => t.name === "list_subscriptions")!;
  assert.deepEqual(Object.keys(listedTool.inputSchema.properties ?? {}), ["session_key"]);
  const l = (await client.callTool({ name: "list_subscriptions", arguments: { session_key: "wsc_test_key" } })) as CallToolResult;
  assert.equal(l.isError, undefined);
  assert.match(text(l), /- es1: crawl\.finished \{"job":"j1"\}, wakes a conversation/);
  const listBody = reqs.find((q) => q.path === "/event-subscriptions/list")!.body;
  assert.equal(listBody.user, undefined);
  assert.equal(listBody.call.session, "wsc_test_key");
  assert.equal(listBody.call.tool, "list_subscriptions");
  const u = (await client.callTool({ name: "unsubscribe_events", arguments: { id: "es1", session_key: "wsc_test_key" } })) as CallToolResult;
  assert.match(text(u), /Stopped subscription es1/);
  const endBody = reqs.find((q) => q.path === "/event-subscriptions/end")!.body;
  assert.equal(endBody.id, "es1");
  assert.equal(endBody.user, undefined);
  assert.equal(endBody.call.session, "wsc_test_key");
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
  assert.equal(wireResult(client).resultType, "complete");
  assert.equal(list.events[0].name, "chat.message");
  assert.deepEqual(list.events[0].delivery, ["webhook", "poll"]);

  const delivery = { mode: "webhook", url: "https://chatgpt.example/cb", secret: "whsec_x" };
  const res = (await client.request(
    { method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "eng" }, delivery, ttlMs: 3600000 } } as any,
    z.any() as any,
  )) as any;
  assert.deepEqual(wireResult(client), { resultType: "complete", id: "sub_1", refreshBefore: "2026-10-22T10:00:00Z", cursor: "c1", truncated: false });
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
  void un;
  assert.deepEqual(wireResult(client), { resultType: "complete" });
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
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fakeFetch, logger: warningsInto(warnings) });
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
  // Rate-limited (warnings.json authorize_failed): one line for both, without the error's message.
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /authorize for chat\.message threw/);
  assert.ok(!warnings[0].includes("ACL service is down"), warnings[0]);
  assert.equal(reqs.filter((u) => u.includes("subscribe")).length, 0, "nothing reached the core");
});

test("builtinTools picks which of Wakeshift's own tools attach adds: all three by default; unknown keys and values throw (disabled-client.json option_checks)", async () => {
  const listed = async (builtinTools?: AttachOptions["builtinTools"]) => {
    const { wake, warnings } = fakeCore();
    chatEvent(wake);
    const server = new McpServer({ name: "chat", version: "1.0.0" });
    server.registerTool("ping", { inputSchema: {} }, async () => ({ content: [] }));
    wake.attach(server, { identify: () => ({ user: "u1" }), builtinTools });
    const client = await connect(server, "claude-code");
    return { names: (await client.listTools()).tools.map((t) => t.name).sort(), warnings };
  };
  const all = ["list_subscriptions", "ping", "subscribe_events", "unsubscribe_events"];
  for (const b of [undefined, {}, { subscribe: true }, { wait: false }]) {
    const { names, warnings } = await listed(b);
    assert.deepEqual(names, all, JSON.stringify(b));
    assert.deepEqual(warnings, [], JSON.stringify(b));
  }
  assert.deepEqual((await listed({ subscribe: false })).names, ["list_subscriptions", "ping", "unsubscribe_events"]);
  assert.deepEqual((await listed({ subscribe: false, list: false, unsubscribe: false })).names, ["ping"]);
  const { wake } = fakeCore();
  chatEvent(wake);
  const server = () => new McpServer({ name: "x", version: "1.0.0" });
  const config = (says: string) => (e: unknown) => e instanceof UsageError && e.code === "config" && e.message.includes(says);
  assert.throws(() => wake.attach(server(), { builtinTools: { subcribe: false } as any }), config("subcribe"));
  assert.throws(() => wake.attach(server(), { builtinTools: { wait: "yes" } as any }), config("wait"));
  assert.throws(() => wake.attach(server(), { requireUsr: true } as any), config("requireUsr"));
  assert.throws(() => wake.attach(server(), { tools: { wait: { enabled: true } } } as any), config("tools"));
});

test("toolChoice: all three unless false, and the wait tool only when true", () => {
  const all = ["subscribe_events", "list_subscriptions", "unsubscribe_events"];
  assert.deepEqual(toolChoice(undefined), { builtin: new Set(all), wait: false });
  assert.deepEqual(toolChoice({ wait: true }), { builtin: new Set(all), wait: true });
  assert.deepEqual(toolChoice({ list: false }), { builtin: new Set(["subscribe_events", "unsubscribe_events"]), wait: false });
});

test("attach with no events logs no_events (warnings.json attach_no_events); it must come before connecting", async () => {
  const { wake, warnings } = fakeCore();
  const server = new McpServer({ name: "x", version: "1.0.0" });
  wake.attach(server);
  assert.match(warnings.join("\n"), /came before any defineEvent/);
  chatEvent(wake);
  const late = new McpServer({ name: "y", version: "1.0.0" });
  late.registerTool("ping", { inputSchema: {} }, async () => ({ content: [] }));
  const [a] = InMemoryTransport.createLinkedPair();
  await late.connect(a);
  assert.throws(() => wake.attach(late), /before connecting/);
});

test("attach says what it got, when it isn't an McpServer, and what to do for a framework it knows", () => {
  const { wake } = fakeCore();
  chatEvent(wake);
  const refused = (server: unknown): string => {
    try {
      wake.attach(server as McpServer);
    } catch (err) {
      return (err as Error).message;
    }
    assert.fail("attach took it");
  };
  const docs = /https:\/\/wakeshift\.ai\/docs\/frameworks/;
  // Shapes of the frameworks' own objects, as they are at run time.
  class MCPServer {
    toolActions = {};
    startHTTP() {}
    startStdio() {}
    convertTools() {}
  }
  class FastMCP {
    addTool() {}
    start() {}
  }
  class FastMCPSession {
    server = {};
  }
  // Something shaped like a low-level Server, keeping its handlers where Wakeshift can't wrap them.
  class Server {
    setRequestHandler() {}
    connect() {}
  }
  for (const [server, want] of [
    [new MCPServer(), /got a MCPServer\. This looks like Mastra's MCPServer \(@mastra\/mcp\).*use attachMastra from @wakeshift\/sdk\/mastra/],
    [new FastMCP(), /got a FastMCP\. This looks like fastmcp's FastMCP \(punkpeye\).*2026-07-28/],
    [new FastMCPSession(), /This looks like a fastmcp session/],
    [undefined, /got undefined\. Frameworks/],
    [null, /got null\./],
    ["server", /got a string\./],
    [{}, /got an object\./],
  ] as const) {
    const msg = refused(server);
    assert.match(msg, /^wakeshift: attach takes an McpServer or a low-level Server, from @modelcontextprotocol\/server 2\.x or @modelcontextprotocol\/sdk 1\.x/);
    assert.match(msg, want);
    assert.match(msg, docs);
    assert.doesNotMatch(msg, /before connecting/);
  }
  assert.match(refused(new Server()), /this low-level Server keeps its request handlers where Wakeshift can't wrap them/);
});

test("attach blames connecting only when the server is connected", () => {
  const { wake } = fakeCore();
  chatEvent(wake);
  const server = new McpServer({ name: "x", version: "1.0.0" });
  const low = server.server as unknown as { registerCapabilities(c: unknown): void };
  low.registerCapabilities = () => {
    throw new Error("capabilities refused");
  };
  assert.throws(
    () => wake.attach(server),
    (err: Error) => /couldn't add Wakeshift's capabilities to this server: capabilities refused/.test(err.message) && !/before connecting/.test(err.message),
  );
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

// The Go SDK (pkg/wakeshift) writes test/event-tools.json and
// test/events-list.json (WAKESHIFT_UPDATE_GOLDEN=1 go test ./pkg/wakeshift)
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
// inputShape is a JSON Schema as the fixtures compare it (README.md): type,
// properties (each type, description, enum; left out when none), required
// sorted (left out when empty).
function inputShape(s: any): any {
  const properties = Object.fromEntries(
    Object.entries((s.properties ?? {}) as Record<string, any>).map(([k, p]) => [k, { type: p.type, ...(p.description !== undefined ? { description: p.description } : {}), ...(p.enum ? { enum: p.enum } : {}) }]),
  );
  return { type: s.type, ...(Object.keys(properties).length ? { properties } : {}), ...(s.required?.length ? { required: [...s.required].sort() } : {}) };
}

test("the built-in tools' annotations are every SDK's (test/tool-annotations.json)", () => {
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(TOOL_ANNOTATIONS))), goldenFile("tool-annotations.json"));
});

test("the event tools' wording is every SDK's (test/event-tools.json)", () => {
  const want = Object.fromEntries(Object.entries(goldenFile("event-tools.json")).filter(([k]) => !k.startsWith("_")));
  assert.deepEqual(sorted({ ...EVENT_TOOL_TEXT }), sorted(want));
});

// sampleEvents defines events-list.json's sample events on wake.
function sampleEvents(wake: Wakeshift) {
  const chat = wake.defineEvent("chat.message", {
    title: "Chat message",
    description: "A new message in a channel.",
    args: z.object({ channel: z.string().describe("The channel, lowercase"), from: z.string().optional().describe("Only messages from this person") }),
    payload: z.object({ from: z.string().describe("Who wrote it"), text: z.string().describe("The message") }),
    text: (m) => `${m.from}: ${m.text}`,
    public: true,
  });
  const turn = wake.defineEvent("game.turn", {
    description: "It's your move at a table.",
    args: z.object({ table: z.string().describe("The table you're seated at") }),
    payload: z.object({ street: z.string().describe("The betting round"), hand: z.array(z.string()).optional().describe("Your hole cards") }),
    text: (_t, a) => `Your move at ${a.table}`,
    personal: true,
  });
  // Defined out of alphabetical order: arguments and required fields are
  // listed in name order, as the other SDKs list them. Tools-only: the default.
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
  const deploy = wake.defineEvent("deploy.finished", {
    description: "A deploy finished.",
    args: z.object({ env: z.string().describe("The environment") }),
    payload: z.object({ status: z.string().describe("How it ended") }),
    authorize: () => true,
  });
  return { chat, turn, run, deploy };
}

test("events/list, tools/list and the manifest match every SDK's for the same events (test/events-list.json)", async () => {
  const { wake } = fakeCore();
  const { chat, turn, run, deploy } = sampleEvents(wake);
  const server = new McpServer({ name: "sample", version: "1" });
  wake.attach(server, {});
  const client = await connect(server, "claude-code");
  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  const { tools } = await client.listTools();
  const want = goldenFile("events-list.json");
  assert.deepEqual(sorted(JSON.parse(JSON.stringify({ events: list.events, manifest: [chat.manifest, turn.manifest, run.manifest, deploy.manifest] }))), sorted({ events: want.events, manifest: want.manifest }));
  // The built-ins, shape-normalized (README.md), without output schemas (tool-output-schemas.json pins those).
  const got = tools
    .map((t: any) => ({ name: t.name, ...(t.title ? { title: t.title } : {}), description: t.description, inputSchema: inputShape(t.inputSchema), ...(t.annotations ? { annotations: t.annotations } : {}) }))
    .sort((a: any, b: any) => (a.name < b.name ? -1 : 1));
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(got))), sorted(want.tools));
  for (const name of ["subscribe_events", "unsubscribe_events"]) {
    const t: any = tools.find((x: any) => x.name === name);
    assert.deepEqual(t.inputSchema.properties.arguments, want.builtin_arguments_schema, `${name}'s arguments`);
  }
});

test("events/list to a native request with subscribe vouchers lists the tools-only sample with a required wakeshift_voucher (test/events-list.json vouchers)", async () => {
  const want = goldenFile("events-list.json");
  const { wake } = fakeCore({ "/manifest/merge": () => ({ json: { features: ["subscribe_vouchers"] } }), "/app": () => ({ json: { id: "sample", name: want.vouchers.app } }) });
  sampleEvents(wake);
  const server = new McpServer({ name: "sample", version: "1" });
  wake.attach(server, {});
  const native = await connect(server, "openai-mcp");
  const list = (await native.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(list.events))), sorted(want.vouchers.events_native));
  const server2 = new McpServer({ name: "sample", version: "1" });
  wake.attach(server2, {});
  const bridged = await connect(server2, "claude-code");
  const list2 = (await bridged.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.deepEqual(sorted(JSON.parse(JSON.stringify(list2.events))), sorted(want.events), "a bridged request gets the list without tools-only events");
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
      // As the server sent it: the MCP SDK 2.x client drops resultType from a result on older revisions.
      void res;
      assert.deepEqual(wireResult(client), c.result, `${kind}: ${c.name}: result`);
      const { call, dialect: _dialect, ...body } = reqs.find((q) => q.path === `/events/${kind}`)!.body;
      assert.equal(call.user, "u1", `${kind}: ${c.name}: the caller`);
      assert.deepEqual(body, c.core_request, `${kind}: ${c.name}: what Wakeshift is asked`);
      await client.close();
    }
  }
});

const as2026 = (caps: any = {}) => ({
  _meta: {
    "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientInfo": { name: "openclaw", version: "2026.10.10" },
    "io.modelcontextprotocol/clientCapabilities": caps,
  },
});

// Protocol 2026-07-28: on the MCP SDK 1.x (discover.ts) server/discover,
// resultType on every 2026-07-28 result (and only those), the version the
// transport accepts; on 2.x its own. On both, no subscribe_events for a
// request that declares the extension itself.
test("attach: a stateless server answers protocol 2026-07-28 clients", async () => {
  const { wake } = fakeCore();
  chatEvent(wake);
  // A server per request, as a stateless app makes them (2.x: createMcpHandler).
  const make = () => {
    const server = new McpServer({ name: "chat", version: "1.0.0" }, { instructions: "A chat app." });
    wake.attach(server, { identify: () => ({ user: "u1" }) });
    server.registerTool("ping", { description: "Ping" }, async () => ({ content: [{ type: "text", text: "pong" }] }));
    return server;
  };
  const rpc = await rawRpc(make);
  if (MCP === 1) {
    const { SUPPORTED_PROTOCOL_VERSIONS } = await import("@modelcontextprotocol/sdk/types.js");
    assert.ok(SUPPORTED_PROTOCOL_VERSIONS.includes("2026-07-28"), "the transport takes the version's header");
  }

  const d = (await rpc("server/discover", as2026())).result;
  assert.equal(d.resultType, "complete");
  assert.equal(d.supportedVersions[0], "2026-07-28");
  // 1.x's list is the SDK's own (older revisions too); 2.x lists only 2026-07-28 and later there.
  if (MCP === 1) assert.ok(d.supportedVersions.includes("2025-11-25"));
  assert.deepEqual(d.capabilities.events, { listChanged: false });
  assert.deepEqual(d.capabilities.extensions[EXTENSION_KEY], { listChanged: false });
  assert.ok(d.capabilities.tools);
  assert.deepEqual(d._meta["io.modelcontextprotocol/serverInfo"], { name: "chat", version: "1.0.0" });
  assert.equal(d.instructions, "A chat app.");
  assert.equal(d.ttlMs, 0);

  const tools = (await rpc("tools/list", as2026())).result;
  assert.equal(tools.resultType, "complete");
  assert.equal(tools.cacheScope, "private", "tools/list differs per request: this client's only");
  assert.ok(tools.tools.some((t: any) => t.name === "subscribe_events"), "a client that doesn't declare the extension is offered it");
  const nativeTools = (await rpc("tools/list", as2026({ extensions: { [EXTENSION_KEY]: {} } }))).result;
  assert.deepEqual(nativeTools.tools.map((t: any) => t.name).sort(), ["list_subscriptions", "ping", "unsubscribe_events"]);
  // A harness Wakeshift wakes itself (Claude Code) isn't native, whatever it declares: its name wins.
  const claude = as2026({ extensions: { [EXTENSION_KEY]: {} } });
  claude._meta["io.modelcontextprotocol/clientInfo"] = { name: "claude-code", version: "2.1.316" };
  assert.ok((await rpc("tools/list", claude)).result.tools.some((t: any) => t.name === "subscribe_events"), "Claude Code declaring the extension still gets subscribe_events");
  const call = (await rpc("tools/call", { name: "ping", arguments: {}, ...as2026() })).result;
  assert.equal(call.resultType, "complete");
  assert.equal(call.content[0].text, "pong");
  const list = (await rpc("events/list", as2026())).result;
  assert.equal(list.resultType, "complete");

  // An older client's results are as they were.
  const old = (await rpc("tools/list", {})).result;
  assert.equal(old.resultType, undefined);
});

// A vendor that answers server/discover itself, before attach or after:
// its answer stays, with Wakeshift's events capability added.
for (const when of ["before", "after"] as const) {
  test(`attach: a server/discover handler the vendor sets ${when} attach keeps its answer and gains the events capability`, async () => {
    const { wake } = fakeCore();
    chatEvent(wake);
    const vendorDiscover = async () => ({
      ...(MCP === 1 ? { resultType: "complete", ttlMs: 0, cacheScope: "public" } : {}),
      supportedVersions: ["2026-07-28"],
      capabilities: { tools: { listChanged: true }, logging: {}, extensions: { "com.example/own": {} } },
      _meta: { "io.modelcontextprotocol/serverInfo": { name: "vendor", version: "9" } },
      instructions: "The vendor's own.",
    });
    const set = (server: McpServer) => {
      const low = server.server as any;
      if (MCP === 1) low.setRequestHandler(z.object({ method: z.literal("server/discover"), params: z.any().optional() }), vendorDiscover);
      else low.setRequestHandler("server/discover", vendorDiscover);
    };
    const make = () => {
      const server = new McpServer({ name: "chat", version: "1.0.0" });
      if (when === "before") set(server);
      wake.attach(server, { identify: () => ({ user: "u1" }) });
      if (when === "after") set(server);
      server.registerTool("ping", { description: "Ping" }, async () => ({ content: [{ type: "text", text: "pong" }] }));
      return server;
    };
    const rpc = await rawRpc(make);
    const d = (await rpc("server/discover", as2026())).result;
    assert.ok(d, "server/discover answered");
    // 2.x installs its own server/discover when it serves (over the vendor's), from the same capabilities.
    if (MCP === 2) return assert.deepEqual(d.capabilities.events, { listChanged: false });
    assert.equal(d.instructions, "The vendor's own.", "the vendor's answer");
    assert.deepEqual(d._meta["io.modelcontextprotocol/serverInfo"], { name: "vendor", version: "9" });
    assert.deepEqual(d.capabilities.tools, { listChanged: true }, "the vendor's capabilities stay as it gave them");
    assert.deepEqual(d.capabilities.logging, {});
    assert.deepEqual(d.capabilities.extensions["com.example/own"], {});
    assert.deepEqual(d.capabilities.events, { listChanged: false }, "Wakeshift's events capability is added");
    assert.deepEqual(d.capabilities.extensions[EXTENSION_KEY], { listChanged: false });
  });
}

test("attach: the events capability is advertised with any event, tools-only ones included (subscribe vouchers: docs/subscribe-vouchers.md §4.3); none without events", async () => {
  for (const kind of ["none", "tools-only", "open"] as const) {
    const { wake } = fakeCore();
    if (kind !== "none") wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
    if (kind === "open") wake.defineEvent("chat.message", { description: "A message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true });
    const make = () => {
      const server = new McpServer({ name: "crawl", version: "1.0.0" });
      wake.attach(server, { identify: () => ({ user: "u1" }) });
      server.registerTool("ping", { description: "Ping" }, async () => ({ content: [{ type: "text", text: "pong" }] }));
      return server;
    };
    const rpc = await rawRpc(make);
    const caps = (await rpc("server/discover", as2026())).result.capabilities;
    const want = kind !== "none";
    assert.equal(caps.events !== undefined, want, `events capability with ${kind}`);
    assert.equal(caps.extensions?.[EXTENSION_KEY] !== undefined, want);
    assert.ok(caps.tools, "the tools capability either way");
    const init = (await rpc("initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "codex", version: "1" } })).result;
    assert.equal(init.capabilities.events !== undefined, want, "initialize says the same");
  }
});

test("attach: a tools/list cache hint the app set stays, scoped to the client (MCP SDK 2.x)", { skip: MCP !== 2 }, async () => {
  const { wake } = fakeCore();
  chatEvent(wake);
  const make = () => {
    const server = new McpServer({ name: "chat", version: "1.0.0" }, { cacheHints: { "tools/list": { ttlMs: 60000, cacheScope: "public" } } } as any);
    wake.attach(server, { identify: () => ({ user: "u1" }) });
    return server;
  };
  const rpc = await rawRpc(make);
  const tools = (await rpc("tools/list", as2026())).result;
  assert.equal(tools.ttlMs, 60000);
  assert.equal(tools.cacheScope, "private");
});

test("nativeOf: a harness Wakeshift wakes itself is never native, whatever it declares", () => {
  const caps = { extensions: { [EXTENSION_KEY]: {} } };
  const at = (client: string, ua = "", metaCaps = false) => ({
    host: {
      servesEvents: true,
      req: () => ({ meta: metaCaps ? { "io.modelcontextprotocol/clientCapabilities": caps } : {}, header: (n: string) => (n.toLowerCase() === "user-agent" ? ua : "") }),
      clientCapabilities: () => (metaCaps ? undefined : caps),
    },
    clientOf: () => client,
    native: undefined,
    servesEvents: undefined,
  });
  assert.equal(nativeOf(at("acme-agent") as any, {}).native, true, "another client that declares the extension is native");
  assert.equal(nativeOf(at("claude-code") as any, {}).native, false, "initialize's capabilities");
  assert.equal(nativeOf(at("claude-code", "", true) as any, {}).native, false, "_meta's capabilities (2026-07-28)");
  assert.equal(nativeOf(at("", "claude-code/2.1.316 (sdk-cli)", true) as any, {}).native, false, "a stateless server: the User-Agent");
  assert.equal(nativeOf(at("codex-mcp-client") as any, {}).native, false);
  assert.equal(nativeOf(at("openai-mcp") as any, {}).native, true, "ChatGPT stays native");
});
