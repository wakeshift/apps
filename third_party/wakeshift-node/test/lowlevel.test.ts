// attach on a low-level Server, from both MCP SDKs whatever
// WAKESHIFT_TEST_MCP says: @modelcontextprotocol/sdk 1.x's Server
// (setRequestHandler with a zod request schema) and
// @modelcontextprotocol/server 2.x's (setRequestHandler with a method name),
// each with its own hand-written tools/list and tools/call, as vendors write
// them (JSON Schema input, a handler that refuses arguments it doesn't name).
// attach wraps those handlers in place: Wakeshift's own tools, MCP Events and
// the wait tool on the same map; Wakeshift's arguments and the subscribes
// sentence on the tools its wrap option names; their calls stripped of
// Wakeshift's arguments, found by attached.call() (or wake.currentCall()),
// and answered with Wakeshift's block. Python's test_lowlevel.py is the model.

import { test } from "node:test";
import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import assert from "node:assert/strict";
import { Server as V1Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport as V1HttpTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import { InMemoryTransport as V1Memory } from "@modelcontextprotocol/sdk/inMemory.js";
import { Client as V1Client } from "@modelcontextprotocol/sdk/client/index.js";
import { Server as V2Server, createMcpHandler, InMemoryTransport as V2Memory, McpServer as V2McpServer } from "@modelcontextprotocol/server";
import { Client as V2Client } from "@modelcontextprotocol/client";
import { z } from "zod";
import { Wakeshift, isWakeshiftError, UsageError } from "../src/index.js";
import { WAKE_ARGS_JSON } from "../src/toolcall.js";
import { quiet, warningsInto } from "./logs.js";
import { WAIT_TOOL } from "../src/tasks.js";

const seed = Buffer.alloc(32, 9).toString("base64url");
const KEY = `wst_test_cards_${seed}`;
const TRAILER = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)";
const WOKEN = "Wakeshift: this conversation is woken by Monitor.";
const VERSION = "io.modelcontextprotocol/protocolVersion";
const INFO = "io.modelcontextprotocol/clientInfo";
const CAPS = "io.modelcontextprotocol/clientCapabilities";
const TASKS = { extensions: { "io.modelcontextprotocol/tasks": {} } };
const annotations = JSON.parse(readFileSync(new URL("../../test/tool-annotations.json", import.meta.url), "utf8"));
const outputSchemas = JSON.parse(readFileSync(new URL("../../test/tool-output-schemas.json", import.meta.url), "utf8"));
for (const k of Object.keys(outputSchemas)) if (k.startsWith("_")) delete outputSchemas[k];

interface CoreCall {
  path: string;
  body: any;
}

// fakeCore answers the app API's calls, and records them (not the manifest's).
function fakeCore() {
  const calls: CoreCall[] = [];
  const warnings: string[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const reply = (json: unknown) => new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/app":
        return reply({ id: "cards", name: "Shipyard", key: { key_id: "k1", name: "test" } });
      case "/manifest":
      case "/manifest/merge":
        return reply(body ?? { description: "" });
      case "/event-subscriptions":
        return reply({ event_subscription: { id: "sub-1", user: body.call.user, created: true }, session: "wsc_s1", pending: "pa_1", addition: { text: "Subscribed through the tool." } });
      case "/calls/addition":
        return reply({ text: WOKEN, trailer: TRAILER, meta: { "wakeshift/connector": "tok" } });
      case "/event-subscriptions/unsubscribe":
        return reply({ ended: true });
      case "/calls/task":
        return reply({ result: { task: { taskId: "wkt1_x", status: "working" } } });
    }
    return reply({});
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: warningsInto(warnings) });
  const message = wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string() }),
    payload: z.object({ from: z.string(), text: z.string() }),
    text: (m) => `${m.from}: ${m.text}`,
    public: true,
  });
  return { wake, message, calls, warnings, paths: () => calls.map((c) => c.path), bodies: (p: string) => calls.filter((c) => c.path === p).map((c) => c.body) };
}

type Core = ReturnType<typeof fakeCore>;

// The vendor's catalog: JSON Schema in, a handler that refuses any argument
// it doesn't name (as a strict schema would).
const CATALOG = [
  {
    name: "watch",
    description: "Watch a channel.",
    inputSchema: { type: "object", properties: { channel: { type: "string" } }, required: ["channel"], additionalProperties: false },
  },
  { name: "boom", description: "Subscribes, then fails.", inputSchema: { type: "object", properties: { channel: { type: "string" } }, additionalProperties: false } },
  { name: "plain", description: "Echoes its arguments.", inputSchema: { type: "object", properties: { x: { type: "string" } } } },
];

// What each tools/call handler run saw, and does.
interface Seen {
  args: Record<string, unknown>[];
}

type CallFn = (extra: unknown) => { subscribe(ev: any, args: any): Promise<unknown> };

// toolsCall is the vendor's tools/call body, shared by both SDKs' servers.
async function toolsCall(core: Core, seen: Seen, callOf: CallFn, name: string, args: Record<string, unknown>, extra: unknown): Promise<any> {
  seen.args.push(args);
  const def = CATALOG.find((t) => t.name === name);
  if (!def) return { isError: true, content: [{ type: "text", text: `Unknown tool: ${name}` }] };
  const named = Object.keys(def.inputSchema.properties);
  const extraArg = Object.keys(args).find((k) => !named.includes(k));
  if (def.inputSchema.additionalProperties === false && extraArg) return { isError: true, content: [{ type: "text", text: `unexpected argument ${extraArg}` }] };
  if (name === "watch") {
    // An await first: the call is found across the handler's async chain.
    await new Promise((r) => setTimeout(r, 1));
    await callOf(extra).subscribe(core.message, { channel: args.channel });
    return { content: [{ type: "text", text: `Watching #${args.channel}.` }] };
  }
  if (name === "boom") {
    await callOf(extra).subscribe(core.message, { channel: "eng" });
    throw new Error("the crawl didn't start");
  }
  return { content: [{ type: "text", text: JSON.stringify(args) }] };
}

// Each MCP SDK's low-level Server, with the vendor's handlers set.
const SDKS = {
  "1.x": (core: Core, seen: Seen, callOf: CallFn) => {
    const server = new V1Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
    server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: CATALOG as any }));
    server.setRequestHandler(CallToolRequestSchema, async (request, extra) => toolsCall(core, seen, callOf, request.params.name, request.params.arguments ?? {}, extra));
    return server;
  },
  "2.x": (core: Core, seen: Seen, callOf: CallFn) => {
    const server = new V2Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
    server.setRequestHandler("tools/list", async () => ({ tools: CATALOG as any }));
    server.setRequestHandler("tools/call", async (request, ctx) => toolsCall(core, seen, callOf, request.params.name, request.params.arguments ?? {}, ctx));
    return server;
  },
} as const;

// userOf is the bearer token's user, from either SDK's context.
function userOf(extra: any): string | undefined {
  const h = extra?.mcpReq ? extra.http?.req?.headers?.get("authorization") : extra?.requestInfo?.headers?.authorization;
  const auth = extra?.mcpReq ? extra.http?.authInfo?.clientId : extra?.authInfo?.clientId;
  return auth || (typeof h === "string" ? h.replace(/^Bearer /, "") : undefined) || undefined;
}

// stateless serves a server made per request over Streamable HTTP, without
// sessions, attaching each (1.x: its transport, which attach teaches protocol
// 2026-07-28; 2.x: createMcpHandler), and sends 2026-07-28 JSON-RPC to it as
// Claude Code would.
async function stateless(t: { after(fn: () => void): void }, sdk: keyof typeof SDKS, core: Core, seen: Seen, opts: { wait?: boolean } = {}) {
  const callOf: CallFn = () => core.wake.currentCall() as any;
  const make = () => {
    const server = SDKS[sdk](core, seen, callOf);
    core.wake.attach(server as any, {
      identify: (extra: unknown) => ({ user: userOf(extra) }),
      wrap: { watch: [core.message], boom: [core.message] },
      builtinTools: opts.wait ? { wait: true } : undefined,
    });
    return server;
  };
  let url: string;
  if (sdk === "2.x") {
    const handler = createMcpHandler(() => make() as any, { onerror: () => {} });
    const http = createServer(async (req, res) => {
      const body = await readBody(req);
      const r = await handler.fetch(new Request("http://mcp.test/mcp", { method: "POST", headers: req.headers as Record<string, string>, body }));
      res.writeHead(r.status, Object.fromEntries(r.headers));
      res.end(await r.text());
    });
    url = await listen(t, http);
  } else {
    const http = createServer(async (req, res) => {
      const body = JSON.parse(await readBody(req));
      const server = make();
      const transport = new V1HttpTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      res.on("close", () => void transport.close());
      await server.connect(transport);
      await transport.handleRequest(req, res, body);
    });
    url = await listen(t, http);
  }
  let id = 0;
  return async (method: string, params: Record<string, any> = {}, o: { caps?: Record<string, unknown> } = {}) => {
    const name: Record<string, string> = typeof params.name === "string" ? { "mcp-name": params.name } : {};
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: "application/json, text/event-stream",
        authorization: "Bearer ada",
        "user-agent": "claude-cli/2.1.314 (external, cli)",
        "mcp-protocol-version": "2026-07-28",
        "mcp-method": method,
        ...name,
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: ++id,
        method,
        params: { ...params, _meta: { [VERSION]: "2026-07-28", [INFO]: { name: (o.caps?.extensions as any)?.["io.modelcontextprotocol/events"] ? "acme-agent" : "claude-code", version: "2.1.314" }, [CAPS]: o.caps ?? {}, ...(params._meta ?? {}) } },
      }),
    });
    const text = await res.text();
    const json = text.trim().startsWith("{") ? JSON.parse(text) : JSON.parse(text.split("\n").find((l) => l.startsWith("data: "))!.slice(6));
    return json as { result?: any; error?: { code: number; message: string } };
  };
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return Buffer.concat(chunks).toString("utf8");
}

async function listen(t: { after(fn: () => void): void }, http: ReturnType<typeof createServer>): Promise<string> {
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => void http.close());
  return `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
}

const texts = (r: any) => (r?.content ?? []).map((c: any) => c.text ?? "");

for (const sdk of Object.keys(SDKS) as (keyof typeof SDKS)[]) {
  test(`${sdk} low-level Server, stateless: tools/list lists Wakeshift's tools, and its arguments and sentence on the wrapped tools`, async (t) => {
    const core = fakeCore();
    const rpc = await stateless(t, sdk, core, { args: [] }, { wait: true });
    const res = await rpc("tools/list");
    assert.equal(res.error, undefined, JSON.stringify(res.error));
    const tools = res.result.tools as any[];
    const byName = new Map(tools.map((t) => [t.name, t]));
    assert.deepEqual(
      tools.map((t) => t.name),
      ["watch", "boom", "plain", "subscribe_events", "unsubscribe_events", "list_subscriptions"],
    );
    const watch = byName.get("watch");
    assert.deepEqual(watch.inputSchema, { ...CATALOG[0].inputSchema, properties: { ...WAKE_ARGS_JSON, channel: { type: "string" } } });
    assert.match(watch.description, /^Watch a channel\. This tool is how Shipyard notifies you in this conversation of this event: A new message in a channel\. Shipyard delivers/);
    assert.deepEqual(byName.get("plain").inputSchema, CATALOG[2].inputSchema, "a tool attach doesn't wrap is left alone");
    assert.equal(byName.get("plain").description, "Echoes its arguments.");
    // The compact scope: all three on subscribe_events, session_key on unsubscribe_events and list_subscriptions.
    const scope: Record<string, string[]> = { subscribe_events: ["conversation_id", "harness", "session_key"], unsubscribe_events: ["session_key"], list_subscriptions: ["session_key"] };
    for (const name of ["subscribe_events", "unsubscribe_events", "list_subscriptions"]) {
      assert.deepEqual(byName.get(name).annotations, annotations[name], name);
      assert.deepEqual(byName.get(name).outputSchema, outputSchemas[name], name);
      assert.deepEqual(Object.keys(byName.get(name).inputSchema.properties ?? {}).filter((k) => k in WAKE_ARGS_JSON).sort(), scope[name], name);
    }
    // A request that takes MCP tasks sees the wait tool too, and one that
    // speaks MCP Events isn't offered subscribe_events.
    const withTasks = (await rpc("tools/list", {}, { caps: TASKS })).result.tools.map((t: any) => t.name);
    assert.ok(withTasks.includes(WAIT_TOOL), withTasks.join(", "));
    const native = (await rpc("tools/list", {}, { caps: { extensions: { "io.modelcontextprotocol/events": {} } } })).result.tools.map((t: any) => t.name);
    assert.ok(!native.includes("subscribe_events"), native.join(", "));
  });

  test(`${sdk} low-level Server, stateless: a wrapped call reaches the handler without Wakeshift's arguments, subscribes through wake.currentCall(), and gets the block`, async (t) => {
    const core = fakeCore();
    const seen: Seen = { args: [] };
    const rpc = await stateless(t, sdk, core, seen);
    const res = await rpc("tools/call", { name: "watch", arguments: { channel: "eng", harness: "claude-code", session_key: "wsc_given", conversation_id: "c-1" } });
    assert.equal(res.error, undefined, JSON.stringify(res.error));
    assert.equal(res.result.isError, undefined, JSON.stringify(res.result));
    assert.deepEqual(seen.args, [{ channel: "eng" }]);
    assert.deepEqual(texts(res.result), ["Watching #eng.", WOKEN, TRAILER]);
    assert.equal(res.result._meta?.["wakeshift/connector"], "tok");
    assert.deepEqual(core.paths(), ["/event-subscriptions", "/calls/addition"]);
    const [sub] = core.bodies("/event-subscriptions");
    assert.equal(sub.name, "chat.message");
    assert.deepEqual(sub.arguments, { channel: "eng" });
    assert.deepEqual(
      { user: sub.call.user, tool: sub.call.tool, harness: sub.call.harness, session: sub.call.session, conversation: sub.call.conversation, client: sub.call.client, ua: sub.call.user_agent },
      { user: "ada", tool: "watch", harness: "claude-code", session: "wsc_given", conversation: "c-1", client: "claude-code", ua: "claude-cli/2.1.314 (external, cli)" },
    );

    // A tool that doesn't subscribe never sees Wakeshift's arguments (SEC M3), and gets no block.
    core.calls.length = 0;
    const plain = await rpc("tools/call", { name: "plain", arguments: { x: "1", session_key: "wsc_given", harness: "codex" } });
    assert.deepEqual(texts(plain.result), [JSON.stringify({ x: "1" })]);
    assert.deepEqual(core.paths(), []);
  });

  test(`${sdk} low-level Server, stateless: a wrapped call that fails ends what it subscribed`, async (t) => {
    const core = fakeCore();
    const rpc = await stateless(t, sdk, core, { args: [] });
    const res = await rpc("tools/call", { name: "boom", arguments: { channel: "eng" } });
    // The SDK answers a handler that throws its own way (an error result, or a JSON-RPC error).
    assert.ok(res.error || res.result?.isError, JSON.stringify(res));
    assert.deepEqual(core.paths(), ["/event-subscriptions", "/event-subscriptions/unsubscribe"]);
    assert.equal(core.bodies("/event-subscriptions/unsubscribe")[0].reason, "rolled_back");
  });

  test(`${sdk} low-level Server, stateless: events/list, and subscribe_events, on the same server`, async (t) => {
    const core = fakeCore();
    const rpc = await stateless(t, sdk, core, { args: [] });
    const list = await rpc("events/list");
    assert.equal(list.error, undefined, JSON.stringify(list.error));
    assert.deepEqual(
      list.result.events.map((e: any) => e.name),
      ["chat.message"],
    );
    const sub = await rpc("tools/call", { name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } });
    assert.equal(sub.result.isError, undefined, JSON.stringify(sub.result));
    assert.match(texts(sub.result)[0], /^Subscribed to chat\.message \{"channel":"eng"\}\./);
    assert.deepEqual(sub.result.structuredContent, { subscribed: { event: "chat.message", arguments: { channel: "eng" }, created: true }, wakeshift: "Subscribed through the tool." });
    assert.deepEqual(core.paths(), ["/event-subscriptions"]);
  });

  test(`${sdk} low-level Server, connected: wake.currentCall(ctx) finds the call by its context`, async () => {
    const core = fakeCore();
    const seen: Seen = { args: [] };
    const server = SDKS[sdk](core, seen, (extra) => core.wake.currentCall(extra as any) as any);
    core.wake.attach(server as any, { identify: () => ({ user: "ada" }), wrap: { watch: [core.message] } });
    assert.throws(() => core.wake.currentCall(), (e: unknown) => isWakeshiftError(e, "no_call") && /outside a tool call/.test((e as Error).message));
    const res = await connected(sdk, server).then((c) => c.callTool({ name: "watch", arguments: { channel: "eng", harness: "codex" } }));
    assert.deepEqual(seen.args, [{ channel: "eng" }]);
    assert.deepEqual(texts(res), ["Watching #eng.", WOKEN, TRAILER]);
    assert.equal(core.bodies("/event-subscriptions")[0].call.harness, "codex");
  });
}

// connected is a client of each SDK connected to server in memory.
async function connected(sdk: keyof typeof SDKS, server: unknown): Promise<{ callTool(p: { name: string; arguments: Record<string, unknown> }): Promise<any> }> {
  if (sdk === "1.x") {
    const [a, b] = V1Memory.createLinkedPair();
    await (server as V1Server).connect(a);
    const client = new V1Client({ name: "codex-mcp-client", version: "1.0.0" });
    await client.connect(b);
    return client as any;
  }
  const [a, b] = V2Memory.createLinkedPair();
  await (server as V2Server).connect(a);
  const client = new V2Client({ name: "codex-mcp-client", version: "1.0.0" });
  await client.connect(b);
  return client as any;
}

test("a ToolCall from wake.currentCall() ends with its call", async () => {
  const core = fakeCore();
  let kept: { subscribe(ev: any, args: any): Promise<unknown> } | undefined;
  const server = SDKS["2.x"](core, { args: [] }, (extra) => {
    kept = core.wake.currentCall(extra);
    return kept;
  });
  core.wake.attach(server, { identify: () => ({ user: "ada" }), wrap: { watch: [core.message] } });
  await connected("2.x", server).then((c) => c.callTool({ name: "watch", arguments: { channel: "eng" } }));
  await assert.rejects(kept!.subscribe(core.message, { channel: "eng" }), (e: unknown) => isWakeshiftError(e, "call_ended"));
});

test("wrap's events are this client's own", () => {
  const core = fakeCore();
  const other = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const theirs = other.defineEvent("x.y", { description: "X", args: z.object({ a: z.string() }), payload: z.object({}), text: () => "x" });
  assert.throws(
    () => core.wake.attach(SDKS["1.x"](core, { args: [] }, () => ({ subscribe: async () => undefined })), { wrap: { watch: [theirs] } }),
    (e: unknown) => e instanceof UsageError && /another client's event/.test(e.message),
  );
});

test("a wrapped call answered with a task: rolled back, and a warning only if it subscribed", async () => {
  const core = fakeCore();
  // A low-level server as attach sees one: its handler map, called directly.
  const handlers = new Map<string, (req: any, ctx: any) => Promise<any>>();
  let subscribeFirst = false;
  handlers.set("tools/list", async () => ({ tools: [{ name: "crawl", inputSchema: { type: "object" } }] }));
  handlers.set("tools/call", async () => {
    if (subscribeFirst) await core.wake.currentCall().subscribe(core.message, { channel: "eng" });
    return { task: { taskId: "t1", status: "working" } };
  });
  const low = { _requestHandlers: handlers, setRequestHandler() {}, connect() {}, registerCapabilities() {}, getClientVersion: () => undefined, getClientCapabilities: () => undefined };
  core.wake.attach(low as any, { identify: () => ({ user: "ada" }), wrap: { crawl: [core.message] } });
  const call = () => handlers.get("tools/call")!({ method: "tools/call", params: { name: "crawl", arguments: { session_key: "wsc_x" } } }, {});
  assert.deepEqual(await call(), { task: { taskId: "t1", status: "working" } });
  assert.deepEqual(core.warnings, [], "nothing subscribed: no warning");
  subscribeFirst = true;
  await call();
  assert.deepEqual(core.paths(), ["/event-subscriptions", "/event-subscriptions/unsubscribe"]);
  assert.match(core.warnings.join("\n"), /crawl answered with something other than a tool result/);
});
