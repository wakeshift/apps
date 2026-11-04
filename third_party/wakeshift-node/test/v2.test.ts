// What only @modelcontextprotocol/server 2.x has: protocol 2026-07-28
// served natively through createMcpHandler (a server per request), where a
// request's context is 2.x's ctx (the reserved io.modelcontextprotocol/*
// _meta keys lifted into mcpReq.envelope, the token in http.authInfo, the
// headers on http.req). Wakeshift must hear the same call as from 1.x. The
// rest of the suite runs against 2.x too (WAKESHIFT_TEST_MCP=v2,
// test/mcp.ts); these run whichever MCP SDK the rest runs on.

import { test } from "node:test";
import assert from "node:assert/strict";
import { McpServer, createMcpHandler, type ServerContext } from "@modelcontextprotocol/server";
import { z } from "zod";
import { Wakeshift, type Attached } from "../src/index.js";
import { EXTENSION_KEY } from "../src/spec.js";
import { hostOf } from "../src/host.js";
import { WAIT_TOOL } from "../src/tasks.js";

const seed = Buffer.alloc(32, 5).toString("base64url");
const VERSION = "io.modelcontextprotocol/protocolVersion";
const INFO = "io.modelcontextprotocol/clientInfo";
const CAPS = "io.modelcontextprotocol/clientCapabilities";

interface CoreReq {
  path: string;
  body: any;
}

function fakeCore(answers: Record<string, (body: any) => unknown> = {}) {
  const reqs: CoreReq[] = [];
  const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ path, body });
    const out = answers[path]?.(body) ?? {};
    return new Response(JSON.stringify(out), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { reqs, wake: new Wakeshift({ key: `wst_test_chat_${seed}`, core: "http://core.test", fetch: fakeFetch, onWarning: () => {} }) };
}

const envelope = (caps: Record<string, unknown> = {}) => ({
  [VERSION]: "2026-07-28",
  [INFO]: { name: "claude-code", version: "2.1.314" },
  [CAPS]: caps,
});

// serve is a 2.x HTTP endpoint (createMcpHandler, a server per request) and
// a function that sends one 2026-07-28 request to it, with a bearer token's
// AuthInfo, as an app's auth middleware passes it.
function serve(make: () => McpServer) {
  const handler = createMcpHandler(make, { onerror: () => {} });
  let id = 0;
  return async (method: string, params: Record<string, any>, opts: { caps?: Record<string, unknown>; name?: string; user?: string; ua?: string } = {}) => {
    const headers: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      "mcp-protocol-version": "2026-07-28",
      "mcp-method": method,
      "user-agent": opts.ua ?? "claude-cli/2.1.314 (external, cli)",
    };
    if (opts.name) headers["mcp-name"] = opts.name;
    const body = { jsonrpc: "2.0", id: ++id, method, params: { ...params, _meta: { ...envelope(opts.caps), ...(params._meta ?? {}) } } };
    const res = await handler.fetch(new Request("http://mcp.test/mcp", { method: "POST", headers, body: JSON.stringify(body) }), {
      authInfo: opts.user ? { token: "t", clientId: opts.user, scopes: [] } : undefined,
    });
    const text = await res.text();
    const json = text.trim().startsWith("{") ? JSON.parse(text) : JSON.parse(text.split("\n").find((l) => l.startsWith("data: "))!.slice(6));
    return json as { result?: any; error?: { code: number; message: string } };
  };
}

function chatApp(wake: Wakeshift) {
  return wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string() }),
    payload: z.object({ from: z.string(), text: z.string() }),
    text: (m) => `${m.from}: ${m.text}`,
  });
}

test("2.x: attach takes 2.x's McpServer, and identify gets its ctx", () => {
  const { wake } = fakeCore();
  chatApp(wake);
  const server = new McpServer({ name: "chat", version: "1" });
  assert.equal(hostOf(server).version, 2);
  const attached: Attached<ServerContext> = wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }) });
  assert.ok(attached);
  assert.throws(() => hostOf({}), /attach takes an McpServer/);
});

test("2.x on 2026-07-28: a wrapped tool's call reaches Wakeshift as from 1.x, and its result carries the block", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1", agent: { harness: "claude-code", via: "client" } }),
    "/calls/addition": () => ({ text: "Wakeshift: wait with wakeshift_wait.", meta: { "wakeshift/hint": "x" } }),
  });
  const message = chatApp(wake);
  let seen: ServerContext | undefined;
  const rpc = serve(() => {
    const server = new McpServer({ name: "chat", version: "1" });
    const attached = wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }), tools: { wait: { enabled: true } } });
    server.registerTool(
      "watch",
      { description: "Watch a channel", inputSchema: attached.inputs(z.object({ channel: z.string() })), outputSchema: z.object({ channel: z.string() }) },
      attached.events("watch", async ({ channel }, call) => {
        seen = call.extra;
        await call.subscribe(message, { channel }, { reason: "Watching" });
        return { content: [{ type: "text", text: `Watching ${channel}.` }], structuredContent: { channel } };
      }),
    );
    return server;
  });
  const caps = { extensions: { "io.modelcontextprotocol/tasks": {} } };

  const listed = await rpc("tools/list", {}, { caps, user: "ada" });
  const watch = listed.result.tools.find((t: any) => t.name === "watch");
  assert.ok(watch.inputSchema.properties.session_key, "Wakeshift's arguments are listed");
  assert.equal(watch.outputSchema.properties.wakeshift?.type, "string", "the output schema names wakeshift");
  assert.ok(listed.result.tools.some((t: any) => t.name === WAIT_TOOL), "a client that takes tasks gets the wait tool");
  assert.equal(listed.result.cacheScope, "private");

  const res = await rpc("tools/call", { name: "watch", arguments: { channel: "eng", session_key: "wsc_0" } }, { caps, name: "watch", user: "ada" });
  assert.equal(res.error, undefined, JSON.stringify(res.error));
  assert.equal(res.result.content[0].text, "Watching eng.");
  assert.equal(res.result.content[1].text, "Wakeshift: wait with wakeshift_wait.");
  assert.deepEqual(res.result.structuredContent, { channel: "eng", wakeshift: "Wakeshift: wait with wakeshift_wait." });
  assert.equal(res.result._meta["wakeshift/hint"], "x");
  assert.ok(seen?.mcpReq, "call.extra is 2.x's ctx");

  const sub = reqs.find((r) => r.path === "/event-subscriptions")!.body;
  // The call as Wakeshift takes it: the envelope's keys back in _meta, the
  // client named by the envelope, the user from identify, tasks from the
  // request's own capabilities.
  assert.deepEqual(sub.call, {
    client: "claude-code",
    user: "ada",
    meta: envelope(caps),
    session: "wsc_0",
    user_agent: "claude-cli/2.1.314 (external, cli)",
    tool: "watch",
    tasks: true,
  });
  assert.deepEqual(sub.arguments, { channel: "eng" });
  assert.equal(reqs.find((r) => r.path === "/calls/addition")!.body.pending, "pa_1");
});

test("2.x on 2026-07-28: a call that fails rolls its subscription back", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1" }),
    "/event-subscriptions/unsubscribe": () => ({ ended: true }),
  });
  const message = chatApp(wake);
  const rpc = serve(() => {
    const server = new McpServer({ name: "chat", version: "1" });
    const attached = wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }) });
    server.registerTool(
      "watch",
      { inputSchema: attached.inputs(z.object({ channel: z.string() })), outputSchema: z.object({ n: z.number() }) },
      attached.events("watch", async ({ channel }, call) => {
        await call.subscribe(message, { channel });
        // Breaks its own output schema: 2.x refuses it, so the subscription goes.
        return { content: [{ type: "text", text: "x" }], structuredContent: { n: "not a number" } };
      }),
    );
    return server;
  });
  const res = await rpc("tools/call", { name: "watch", arguments: { channel: "eng" } }, { name: "watch", user: "ada" });
  assert.ok(res.error || res.result?.isError, "2.x refuses the result");
  const rolled = reqs.find((r) => r.path === "/event-subscriptions/unsubscribe");
  assert.equal(rolled?.body.reason, "rolled_back");
  assert.equal(rolled?.body.pending, "pa_1");
});

test("2.x on 2026-07-28: native events/subscribe and events/poll, with Mcp-Name checked", async () => {
  const { wake, reqs } = fakeCore({
    "/events/subscribe": () => ({ id: "sub_1", refresh_before: "2026-10-22T10:00:00.000Z", cursor: "c1", truncated: false }),
    "/events/poll": () => ({ events: [], cursor: "c2", truncated: false, has_more: false, next_poll_ms: 5000 }),
  });
  chatApp(wake);
  const rpc = serve(() => {
    const server = new McpServer({ name: "chat", version: "1" });
    wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }) });
    return server;
  });
  const native = { extensions: { [EXTENSION_KEY]: {} } };
  const list = await rpc("events/list", {}, { caps: native, user: "ada" });
  assert.equal(list.result.resultType, "complete");
  assert.equal(list.result.events[0].name, "chat.message");

  const delivery = { mode: "webhook", url: "https://client.example/cb", secret: "whsec_x" };
  const sub = await rpc("events/subscribe", { name: "chat.message", arguments: { channel: "eng" }, delivery }, { caps: native, name: "chat.message", user: "ada" });
  assert.equal(sub.error, undefined, JSON.stringify(sub.error));
  assert.equal(sub.result.resultType, "complete");
  assert.equal(sub.result.id, "sub_1");
  const body = reqs.find((r) => r.path === "/events/subscribe")!.body;
  assert.equal(body.call.user, "ada");
  assert.equal(body.call.client, "claude-code");
  assert.deepEqual(body.call.meta, envelope(native), "the envelope's keys reach Wakeshift");

  const mismatch = await rpc("events/subscribe", { name: "chat.message", arguments: { channel: "eng" }, delivery }, { caps: native, name: "other", user: "ada" });
  assert.ok(mismatch.error, "a mismatched Mcp-Name is refused");

  const poll = await rpc("events/poll", { name: "chat.message", arguments: { channel: "eng" } }, { caps: native, name: "chat.message", user: "ada" });
  assert.equal(poll.error, undefined, JSON.stringify(poll.error));
  assert.equal(poll.result.cursor, "c2");
  assert.equal(poll.result.nextPollMs, 5000);

  const noUser = await rpc("events/subscribe", { name: "chat.message", arguments: { channel: "eng" }, delivery }, { caps: native, name: "chat.message" });
  assert.equal(noUser.error?.code, -32024, "no user: Forbidden");

  const tools = await rpc("tools/list", {}, { caps: native, user: "ada" });
  assert.deepEqual(tools.result.tools.map((t: any) => t.name).sort(), ["list_subscriptions", "unsubscribe_events"], "a native client isn't offered subscribe_events");
});

test("2.x on 2026-07-28: the built-in tools, through 2.x's zod 4 registration", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", created: true }, addition: { text: "Woken by Wakeshift." } }),
    "/event-subscriptions/list": () => ({ subscriptions: [] }),
  });
  chatApp(wake);
  const rpc = serve(() => {
    const server = new McpServer({ name: "chat", version: "1" });
    wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.clientId }) });
    return server;
  });
  const res = await rpc("tools/call", { name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } }, { name: "subscribe_events", user: "ada" });
  assert.equal(res.result.content[0].text, 'Subscribed to chat.message {"channel":"eng"}.\n\nWoken by Wakeshift.');
  assert.equal(reqs.find((r) => r.path === "/event-subscriptions")!.body.call.client, "claude-code");
  const bad = await rpc("tools/call", { name: "subscribe_events", arguments: { event: "nope" } }, { name: "subscribe_events", user: "ada" });
  assert.ok(bad.error || bad.result?.isError, "an event it doesn't offer is refused by the input schema");
  const list = await rpc("tools/call", { name: "list_subscriptions", arguments: {} }, { name: "list_subscriptions", user: "ada" });
  assert.equal(list.result.content[0].text, "You have no subscriptions to this app's events.");
});

test("2.x: tasks/get for an ID that isn't Wakeshift's goes to the app's own handler", async () => {
  const { wake, reqs } = fakeCore();
  chatApp(wake);
  const rpc = serve(() => {
    const server = new McpServer({ name: "chat", version: "1" });
    // The app's own tasks/get (the tasks extension's method, registered with a params schema).
    server.server.setRequestHandler("tasks/get", { params: z.looseObject({ taskId: z.string() }) }, async (params) => ({
      taskId: params.taskId,
      status: "completed",
      createdAt: "2026-10-30T12:00:00Z",
      lastUpdatedAt: "2026-10-30T12:00:00Z",
      ttlMs: null,
    }));
    wake.attach(server, { identify: () => ({ user: "ada" }) });
    return server;
  });
  const caps = { extensions: { "io.modelcontextprotocol/tasks": {} } };
  const own = await rpc("tasks/get", { taskId: "app-1" }, { caps, name: "app-1" });
  assert.equal(own.error, undefined, JSON.stringify(own.error));
  assert.equal(own.result.status, "completed");
  assert.ok(!reqs.some((r) => r.path.startsWith("/calls/task")), "never asks Wakeshift");
});

// @modelcontextprotocol/server before 2.3.0 answers tasks/get on 2026-07-28
// itself (modelcontextprotocol/typescript-sdk#2598), so the wait task never
// completes: attach says so, once, naming the version to upgrade to. The
// one installed (the peer range's floor or later) routes them.
test("2.x: attach warns, once, on a server too old to route tasks/get", () => {
  const warnings: string[] = [];
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "http://core.test", onWarning: (m) => warnings.push(m) });
  chatApp(wake);
  wake.attach(new McpServer({ name: "chat", version: "1" }), { tools: { wait: { enabled: true } } });
  assert.deepEqual(warnings, [], "the installed 2.x routes them");
  for (let i = 0; i < 2; i++) {
    const old = new McpServer({ name: "chat", version: "1" });
    // What 2.0 to 2.2 lack: the Set of methods registered with a schema,
    // which 2.3.0's era gate lets through (stubbed, so registering still works).
    (old.server as unknown as Record<string, unknown>)._customSchemaRequestMethods = { add() {}, delete() {}, has: () => false };
    wake.attach(old, { tools: { wait: { enabled: true } } });
  }
  // Without the wait tool there's nothing to route, so nothing to say.
  const quiet: string[] = [];
  const off = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "http://core.test", onWarning: (m) => quiet.push(m) });
  chatApp(off);
  const old = new McpServer({ name: "chat", version: "1" });
  (old.server as unknown as Record<string, unknown>)._customSchemaRequestMethods = { add() {}, delete() {}, has: () => false };
  off.attach(old);
  assert.deepEqual(quiet, []);
  assert.equal(warnings.length, 1, warnings.join("\n"));
  assert.match(warnings[0], /@modelcontextprotocol\/server is older than 2\.3\.0/);
  assert.match(warnings[0], /Upgrade @modelcontextprotocol\/server to 2\.3\.0 or later/);
});
