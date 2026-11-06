// @wakeshift/sdk/mastra against a stand-in for Mastra's MCPServer built the
// way @mastra/mcp 2.x builds its servers: a low-level Server
// (@modelcontextprotocol/server 2.x) per request, from createServerInstance,
// with its own tools/list and tools/call (a Mastra tool's execute gets
// { mcp: { extra: { ...ctx } } }) and, optionally, its own MCP Events. The
// real MCPServer runs in sdk/compat (ts-mastra), pinned and on its newest
// release.

import { test } from "node:test";
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { Server, createMcpHandler } from "@modelcontextprotocol/server";
import { z } from "zod";
import * as z4 from "zod/v4";
import { Wakeshift } from "../src/index.js";
import { quiet, warningsInto } from "./logs.js";
import { attachMastra, supportedMastra } from "../src/mastra.js";
import { EXTENSION_KEY } from "../src/spec.js";
import { WAIT_TOOL } from "../src/tasks.js";

const seed = Buffer.alloc(32, 7).toString("base64url");
const VERSION = "io.modelcontextprotocol/protocolVersion";
const INFO = "io.modelcontextprotocol/clientInfo";
const CAPS = "io.modelcontextprotocol/clientCapabilities";
const TASKS = { extensions: { "io.modelcontextprotocol/tasks": {} } };

interface CoreReq {
  path: string;
  body: any;
}

function fakeCore(answers: Record<string, (body: any) => unknown> = {}) {
  const reqs: CoreReq[] = [];
  const warnings: string[] = [];
  const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ path, body });
    const out = answers[path]?.(body) ?? {};
    return new Response(JSON.stringify(out), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "https://core.test", fetch: fakeFetch, logger: warningsInto(warnings) });
  return { reqs, warnings, wake };
}

type MastraTool = { description: string; inputSchema: z4.ZodObject; execute(input: any, context: any): Promise<unknown> };

// MCPServer is the stand-in: what @mastra/mcp's createServerInstance builds.
class MCPServer {
  builds = 0;
  constructor(
    private readonly tools: Record<string, MastraTool>,
    private readonly ownEvents = false,
  ) {}
  startHTTP() {}
  startStdio() {}
  convertTools() {}
  createServerInstance(): Server {
    this.builds++;
    const caps: Record<string, unknown> = { tools: {} };
    if (this.ownEvents) caps.extensions = { [EXTENSION_KEY]: {} };
    const server = new Server({ name: "mastra-app", version: "1.0.0" }, { capabilities: caps });
    server.setRequestHandler("tools/list", async () => ({
      tools: Object.entries(this.tools).map(([name, t]) => ({ name, description: t.description, inputSchema: z4.toJSONSchema(t.inputSchema) as any })),
    }));
    server.setRequestHandler("tools/call", async (request, ctx) => {
      const t = this.tools[request.params.name];
      if (!t) return { isError: true, content: [{ type: "text", text: `Unknown tool: ${request.params.name}` }] };
      const input = t.inputSchema.parse(request.params.arguments ?? {});
      try {
        const out = await t.execute(input, { mcp: { extra: { ...ctx, signal: ctx.mcpReq.signal, _meta: ctx.mcpReq._meta } } });
        return { isError: false, content: [{ type: "text", text: typeof out === "string" ? out : JSON.stringify(out) }] };
      } catch (err) {
        return { isError: true, content: [{ type: "text", text: String((err as Error).message) }] };
      }
    });
    if (this.ownEvents) {
      server.setRequestHandler("events/list", { params: z4.looseObject({}) }, async () => ({
        events: [{ name: "build.done", description: "A build finished", delivery: ["poll"], inputSchema: { type: "object" } }],
      }));
      server.setRequestHandler("events/poll", { params: z4.looseObject({}) }, async (params: any) => ({ events: [], cursor: `mastra:${params.name}` }));
    }
    return server;
  }
}

function chatApp(wake: Wakeshift) {
  return wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string() }),
    payload: z.object({ from: z.string(), text: z.string() }),
    text: (m) => `${m.from}: ${m.text}`,
    public: true,
  });
}

// serve sends 2026-07-28 requests to the MCPServer through 2.x's
// createMcpHandler, as MCPServer's startHTTP does.
function serve(server: MCPServer) {
  const handler = createMcpHandler(() => server.createServerInstance(), { onerror: () => {} });
  let id = 0;
  return async (method: string, params: Record<string, any>, opts: { caps?: Record<string, unknown>; user?: string; client?: string } = {}) => {
    const headers: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      "mcp-protocol-version": "2026-07-28",
      "mcp-method": method,
      "user-agent": "claude-cli/2.1.314 (external, cli)",
    };
    if (typeof params.name === "string") headers["mcp-name"] = params.name;
    if (typeof params.taskId === "string") headers["mcp-name"] = params.taskId;
    const meta = { [VERSION]: "2026-07-28", [INFO]: { name: opts.client ?? "claude-code", version: "2.1.314" }, [CAPS]: opts.caps ?? {} };
    const body = { jsonrpc: "2.0", id: ++id, method, params: { ...params, _meta: { ...meta, ...(params._meta ?? {}) } } };
    const res = await handler.fetch(new Request("http://mcp.test/mcp", { method: "POST", headers, body: JSON.stringify(body) }), {
      authInfo: opts.user ? { token: "t", clientId: opts.user, scopes: [] } : undefined,
    });
    const text = await res.text();
    const json = text.trim().startsWith("{") ? JSON.parse(text) : JSON.parse(text.split("\n").find((l) => l.startsWith("data: "))!.slice(6));
    return json as { result?: any; error?: { code: number; message: string } };
  };
}

function watchApp(answers: Record<string, (body: any) => unknown> = {}, ownEvents = false) {
  const core = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1", agent: { harness: "claude-code", via: "client" } }),
    "/calls/addition": () => ({ text: "Wakeshift: run the Monitor command.", trailer: "", meta: { "wakeshift/connector": "beacon" } }),
    "/app": () => ({ id: "chat", name: "Chat" }),
    ...answers,
  });
  const message = chatApp(core.wake);
  const attached = attachMastra(core.wake, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }), builtinTools: { wait: true } });
  let failNext = false;
  const watch: MastraTool = {
    description: "Watch a channel",
    inputSchema: attached.inputs(z4.object({ channel: z4.string() })),
    execute: attached.events("watch", { subscribes: [message] }, async ({ channel }: { channel: string }, call) => {
      await call.subscribe(message, { channel });
      if (failNext) throw new Error("the channel is gone");
      return `Watching ${channel}.`;
    }),
  };
  const ping: MastraTool = { description: "Ping", inputSchema: z4.object({}), execute: async () => "pong" };
  const peek: MastraTool = {
    description: "Peek at a channel",
    inputSchema: attached.inputs(z4.object({ channel: z4.string() })),
    execute: attached.events("peek", { subscribes: [] }, async ({ channel }: { channel: string }) => `Quiet in ${channel}.`),
  };
  const server = attached.serve(new MCPServer({ watch, ping, peek }, ownEvents));
  return { ...core, rpc: serve(server), server, watch, failNext: () => (failNext = true) };
}

test("mastra: tools/list has the app's tools, Wakeshift's own, and the wait tool for a client that takes tasks", async () => {
  const { rpc } = watchApp();
  const listed = await rpc("tools/list", {}, { caps: TASKS, user: "ada" });
  assert.equal(listed.error, undefined, JSON.stringify(listed.error));
  const names = listed.result.tools.map((t: any) => t.name);
  for (const n of ["watch", "ping", "subscribe_events", "unsubscribe_events", "list_subscriptions", WAIT_TOOL]) assert.ok(names.includes(n), `${n} in ${names}`);
  const watch = listed.result.tools.find((t: any) => t.name === "watch");
  assert.ok(watch.inputSchema.properties.session_key, "the wrapped tool takes Wakeshift's arguments");
  // It declares chat.message: tools/list says it's how the app notifies, in the event's words.
  assert.equal(
    watch.description,
    "Watch a channel. This tool is how Chat notifies you in this conversation of this event: A new message in a channel. Chat delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.",
  );
  assert.equal(listed.result.tools.find((t: any) => t.name === "ping").description, "Ping");
  // Compact scoping: a tool that subscribes to nothing lists only session_key, and still takes all three.
  const peek = listed.result.tools.find((t: any) => t.name === "peek");
  assert.deepEqual(Object.keys(peek.inputSchema.properties).sort(), ["channel", "session_key"]);
  assert.deepEqual(Object.keys(watch.inputSchema.properties).sort(), ["channel", "conversation_id", "harness", "session_key"]);
  const peeked = await rpc("tools/call", { name: "peek", arguments: { channel: "c", harness: "claude-code", conversation_id: "x" } }, { user: "ada" });
  assert.equal(peeked.error, undefined, JSON.stringify(peeked.error));
  assert.ok(JSON.stringify(peeked.result.content).includes("Quiet in c."));
  const sub = listed.result.tools.find((t: any) => t.name === "subscribe_events");
  assert.deepEqual(sub.inputSchema.properties.event.enum, ["chat.message"]);
  // Wakeshift's own tools and the wait tool carry every SDK's annotations.
  const annotations = JSON.parse(readFileSync(new URL("../../test/tool-annotations.json", import.meta.url), "utf8"));
  for (const name of Object.keys(annotations)) {
    assert.deepEqual(listed.result.tools.find((t: any) => t.name === name).annotations, annotations[name], name);
  }
  assert.equal(listed.result.cacheScope, "private");

  // A client that speaks MCP Events subscribes natively: no subscribe_events, no wait tool.
  const native = await rpc("tools/list", {}, { caps: { extensions: { [EXTENSION_KEY]: {} } }, client: "acme-agent" });
  const nativeNames = native.result.tools.map((t: any) => t.name);
  assert.ok(!nativeNames.includes("subscribe_events"));
  assert.ok(!nativeNames.includes(WAIT_TOOL));
  assert.ok(nativeNames.includes("unsubscribe_events"));
});

test("mastra: a wrapped tool's call subscribes the caller, and its result carries the block and _meta", async () => {
  const { rpc, reqs, warnings } = watchApp();
  const res = await rpc("tools/call", { name: "watch", arguments: { channel: "eng", session_key: "wsc_0" } }, { user: "ada" });
  assert.equal(res.error, undefined, JSON.stringify(res.error));
  assert.equal(res.result.isError, false);
  assert.equal(res.result.content[0].text, "Watching eng.");
  assert.equal(res.result.content[1].text, "Wakeshift: run the Monitor command.");
  assert.equal(res.result._meta["wakeshift/connector"], "beacon");
  const sub = reqs.find((r) => r.path === "/event-subscriptions")!.body;
  assert.equal(sub.call.client, "claude-code");
  assert.equal(sub.call.user, "ada");
  assert.equal(sub.call.session, "wsc_0");
  assert.equal(sub.call.tool, "watch");
  assert.deepEqual(sub.arguments, { channel: "eng" });
  assert.equal(reqs.find((r) => r.path === "/calls/addition")!.body.pending, "pa_1");
  assert.deepEqual(warnings, [], "a declared subscribe doesn't warn");
});

test("mastra: a wrapped tool that fails rolls its subscription back", async () => {
  const app = watchApp();
  app.failNext();
  const res = await app.rpc("tools/call", { name: "watch", arguments: { channel: "eng" } }, { user: "ada" });
  assert.equal(res.result.isError, true);
  assert.equal(res.result.content.length, 1, "no block on a failed call");
  const rolled = app.reqs.find((r) => r.path === "/event-subscriptions/unsubscribe")!.body;
  assert.equal(rolled.reason, "rolled_back");
  assert.equal(rolled.pending, "pa_1");
});

test("mastra: subscribe_events and the wait tool reach Wakeshift with the caller, and other tools are untouched", async () => {
  const { rpc, reqs } = watchApp({
    "/event-subscriptions": () => ({ event_subscription: { id: "es2", created: true }, addition: { text: "Woken by Monitor.", meta: { "wakeshift/connector": "b2" } } }),
    "/calls/task": () => ({ result: { resultType: "task", task: { taskId: "wkt1_abc", status: "working" } } }),
  });
  const sub = await rpc("tools/call", { name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } }, { user: "ada" });
  assert.equal(sub.error, undefined, JSON.stringify(sub.error));
  assert.match(sub.result.content[0].text, /^Subscribed to chat\.message \{"channel":"eng"\}\.\n\nWoken by Monitor\.$/);
  assert.equal(sub.result._meta["wakeshift/connector"], "b2");
  assert.equal(reqs.find((r) => r.path === "/event-subscriptions")!.body.call.user, "ada");

  const wait = await rpc("tools/call", { name: WAIT_TOOL, arguments: { session_key: "wsc_1" } }, { caps: TASKS, user: "ada" });
  assert.equal(wait.error, undefined, JSON.stringify(wait.error));
  assert.equal(reqs.find((r) => r.path === "/calls/task")!.body.call.session, "wsc_1");

  const ping = await rpc("tools/call", { name: "ping", arguments: {} }, { user: "ada" });
  assert.deepEqual(ping.result.content, [{ type: "text", text: "pong" }]);
});

test("mastra: Wakeshift's MCP Events and Mastra's own share events/*", async () => {
  const { rpc, reqs } = watchApp({ "/events/poll": () => ({ events: [], cursor: "wk1", truncated: false, has_more: false, next_poll_ms: 1000 }) }, true);
  const listed = await rpc("events/list", {}, { client: "openai-mcp" });
  assert.equal(listed.error, undefined, JSON.stringify(listed.error));
  assert.deepEqual(
    listed.result.events.map((e: any) => e.name),
    ["chat.message", "build.done"],
  );
  const mastra = await rpc("events/poll", { name: "build.done", arguments: {} }, { user: "ada" });
  assert.equal(mastra.result.cursor, "mastra:build.done");
  const ours = await rpc("events/poll", { name: "chat.message", arguments: { channel: "eng" } }, { user: "ada" });
  assert.equal(ours.error, undefined, JSON.stringify(ours.error));
  assert.equal(ours.result.cursor, "wk1");
  assert.equal(reqs.filter((r) => r.path === "/events/poll").length, 1);
});

test("mastra: a wrapped tool run outside an MCP call can't subscribe", async () => {
  const { watch } = watchApp();
  await assert.rejects(watch.execute({ channel: "eng" }, {}), /outside an MCP client's call/);
});

test("mastra: serve takes MCPServer only, once, and checks @mastra/mcp's version", () => {
  const { wake } = fakeCore();
  chatApp(wake);
  const attached = attachMastra(wake);
  assert.throws(() => attached.serve({}), /takes Mastra's MCPServer/);
  const server = new MCPServer({});
  const build = server.createServerInstance;
  attached.serve(server);
  const once = server.createServerInstance;
  assert.notEqual(once, build);
  attached.serve(server);
  assert.equal(server.createServerInstance, once, "served once");
  for (const v of ["2.7.0", "2.7.3", "2.12.0"]) assert.ok(supportedMastra(v), v);
  for (const v of ["2.6.9", "1.9.0", "3.0.0", "x"]) assert.ok(!supportedMastra(v), v);
});

test("mastra: attachMastra with no events logs no_events; it refuses unknown options and values", () => {
  const { wake, warnings } = fakeCore();
  attachMastra(wake);
  assert.match(warnings.join("\n"), /came before any defineEvent/);
  chatApp(wake);
  assert.throws(() => attachMastra(wake, { builtinTools: { subcribe: false } as any }), /subcribe/);
});

test("mastra: a server Wakeshift can't attach to is served without it, with one warning", async () => {
  const { wake, warnings } = fakeCore();
  chatApp(wake);
  const server = new MCPServer({});
  server.createServerInstance = () => ({}) as Server;
  attachMastra(wake).serve(server);
  server.createServerInstance();
  server.createServerInstance();
  assert.equal(warnings.filter((w) => /served without Wakeshift/.test(w)).length, 1);
});
