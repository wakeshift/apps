// attach at the protocol layer, hardened (design §2.3, §8 PR 4): handlers
// set again after attach are wrapped again (CR H2); a second attach and an
// McpServer's inner server are refused (CR M1, M3); a vendor tool named like
// a built-in loses to it, logged (CR L1); a tool in wrap whose own schema
// names a wake argument is served unwrapped, logged (CR L4); a wrap name the
// server never lists is logged (CR L2); 1.x needs require(esm) (CR M4).
import { test } from "node:test";
import assert from "node:assert/strict";
import { Server as V1Server } from "@modelcontextprotocol/sdk/server/index.js";
import { McpServer as V1McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { CallToolRequestSchema, ListToolsRequestSchema, ToolListChangedNotificationSchema } from "@modelcontextprotocol/sdk/types.js";
import { InMemoryTransport as V1Memory } from "@modelcontextprotocol/sdk/inMemory.js";
import { Client as V1Client } from "@modelcontextprotocol/sdk/client/index.js";
import { Server as V2Server, InMemoryTransport as V2Memory } from "@modelcontextprotocol/server";
import { Client as V2Client } from "@modelcontextprotocol/client";
import { z } from "zod";
import { UsageError, Wakeshift } from "../src/index.js";
import { captureLog } from "./logs.js";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

function app() {
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const reply = (json: unknown) => new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/app":
        return reply({ id: "cards", name: "Shipyard", key: { key_id: "k1", name: "test" } });
      case "/event-subscriptions":
        return reply({ event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" });
      case "/calls/addition":
        return reply({ text: "Woken by Monitor." });
      case "/event-subscriptions/list":
        return reply({ subscriptions: [] });
    }
    return reply(body ?? {});
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
  const message = wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string() }),
    payload: z.object({ text: z.string() }),
    text: (m) => m.text,
  });
  return { wake, message, calls, log };
}

type Tool = { name: string; description: string; inputSchema: Record<string, unknown> };

// lowServer is a low-level Server of either SDK with the vendor's handlers set.
function lowServer(sdk: "1.x" | "2.x", tools: Tool[], call: (name: string, args: Record<string, unknown>) => Promise<any>) {
  if (sdk === "1.x") {
    const s = new V1Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
    s.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: tools as any }));
    s.setRequestHandler(CallToolRequestSchema, async (r) => call(r.params.name, r.params.arguments ?? {}));
    return {
      server: s as any,
      resetCall: (fn: typeof call) => s.setRequestHandler(CallToolRequestSchema, async (r) => fn(r.params.name, r.params.arguments ?? {})),
      resetList: (list: Tool[]) => s.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: list as any })),
    };
  }
  const s = new V2Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
  s.setRequestHandler("tools/list", async () => ({ tools: tools as any }));
  s.setRequestHandler("tools/call", async (r) => call(r.params.name, r.params.arguments ?? {}));
  return {
    server: s as any,
    resetCall: (fn: typeof call) => s.setRequestHandler("tools/call", async (r) => fn(r.params.name, r.params.arguments ?? {})),
    resetList: (list: Tool[]) => s.setRequestHandler("tools/list", async () => ({ tools: list as any })),
  };
}

async function connect(sdk: "1.x" | "2.x", server: any): Promise<V1Client> {
  if (sdk === "1.x") {
    const [a, b] = V1Memory.createLinkedPair();
    await server.connect(a);
    const client = new V1Client({ name: "claude-code", version: "1" });
    await client.connect(b);
    return client;
  }
  const [a, b] = V2Memory.createLinkedPair();
  await server.connect(a);
  const client = new V2Client({ name: "claude-code", version: "1" });
  await client.connect(b as any);
  return client as unknown as V1Client;
}

const WATCH: Tool = { name: "watch", description: "Watch a channel.", inputSchema: { type: "object", properties: { channel: { type: "string" } }, required: ["channel"] } };
const text = (r: any) => (r.content ?? []).map((c: any) => c.text ?? "").join("\n");

for (const sdk of ["1.x", "2.x"] as const) {
  test(`${sdk} low-level Server: a tools/call handler set again after attach is wrapped again (CR H2)`, async () => {
    const a = app();
    const seen: Record<string, unknown>[] = [];
    const low = lowServer(sdk, [WATCH], async () => ({ content: [{ type: "text", text: "first" }] }));
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message] } });
    // The vendor re-registers its handler (middleware, hot reload): Wakeshift stays.
    low.resetCall(async (_name, args) => {
      seen.push(args);
      await a.wake.currentCall().subscribe(a.message, { channel: String(args.channel) });
      return { content: [{ type: "text", text: "second" }] };
    });
    const client = await connect(sdk, low.server);
    const listed = (await client.listTools()).tools.find((t) => t.name === "watch")!;
    assert.ok("session_key" in (listed.inputSchema.properties as object), "still advertised");
    const r = await client.callTool({ name: "watch", arguments: { channel: "eng", session_key: "wsc_canary" } });
    assert.deepEqual(seen, [{ channel: "eng" }], "the handler set again never sees session_key");
    assert.match(text(r), /second/);
    assert.match(text(r), /Woken by Monitor/, "and the result still gets the block");
    assert.ok(a.calls.some((c) => c.path === "/event-subscriptions"));
  });

  test(`${sdk} low-level Server: a handler set after attach that calls the one it replaced amends tools/list once and still wraps the call`, async () => {
    const a = app();
    const seen: Record<string, unknown>[] = [];
    const low = lowServer(sdk, [WATCH], async (_name, args) => {
      seen.push(args);
      await a.wake.currentCall().subscribe(a.message, { channel: String(args.channel) });
      return { content: [{ type: "text", text: "watching" }] };
    });
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message] } });
    // The vendor's pattern: replace tools/list (and tools/call) with handlers
    // that call the ones attach installed, and change the result.
    const handlers: Map<string, (r: any, x: any) => Promise<any>> = (low.server as any)._requestHandlers;
    const oldList = handlers.get("tools/list")!;
    const oldCall = handlers.get("tools/call")!;
    const list = async (r: any, x: any) => {
      const out = await oldList(r, x);
      return { ...out, tools: out.tools.map((t: any) => ({ ...t, title: t.name.toUpperCase() })) };
    };
    const call = async (r: any, x: any) => oldCall(r, x);
    if (sdk === "1.x") {
      low.server.setRequestHandler(ListToolsRequestSchema, list);
      low.server.setRequestHandler(CallToolRequestSchema, call);
    } else {
      low.server.setRequestHandler("tools/list", list);
      low.server.setRequestHandler("tools/call", call);
    }
    const client = await connect(sdk, low.server);
    // A stateful session lists more than once: each list is amended once.
    await client.listTools();
    const listed = (await client.listTools()).tools.find((t) => t.name === "watch")!;
    assert.equal(listed.title, "WATCH", "the vendor's change is kept");
    assert.ok("session_key" in (listed.inputSchema.properties as object));
    assert.equal(listed.description!.split("This tool is how").length, 2, "the subscribes sentence appears once");
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "wrap_arg_conflict"), [], "no conflict: the schema names session_key because Wakeshift added it");
    const r = await client.callTool({ name: "watch", arguments: { channel: "eng", session_key: "wsc_canary" } });
    assert.deepEqual(seen, [{ channel: "eng" }], "the tool never sees session_key");
    assert.match(text(r), /watching/);
    assert.match(text(r), /Woken by Monitor/, "the result gets the block, once");
    assert.equal(text(r).split("Woken by Monitor").length, 2);
    assert.equal(a.calls.filter((c) => c.path === "/event-subscriptions").length, 1);
  });

  test(`${sdk} low-level Server: wrap as a function names tools that subscribe at run time`, async () => {
    const a = app();
    const seen: Record<string, unknown>[] = [];
    const actor = (n: string): Tool => ({ name: `run_${n}`, description: `Run ${n}.`, inputSchema: { type: "object", properties: { channel: { type: "string" } } } });
    const tools = [actor("scraper"), { name: "plain", description: "Plain.", inputSchema: { type: "object", properties: {} } }];
    const low = lowServer(sdk, tools, async (name, args) => {
      seen.push(args);
      if (name.startsWith("run_")) await a.wake.currentCall().subscribe(a.message, { channel: String(args.channel) });
      return { content: [{ type: "text", text: `ran ${name}` }] };
    });
    const asked: string[] = [];
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: (tool) => (asked.push(tool), tool.startsWith("run_") ? [a.message] : undefined) });
    const client = await connect(sdk, low.server);
    // A tool added after attach, named at run time.
    tools.push(actor("crawler"));
    const listed = (await client.listTools()).tools;
    for (const n of ["run_scraper", "run_crawler"]) {
      const t = listed.find((x) => x.name === n)!;
      assert.ok("session_key" in (t.inputSchema.properties as object), `${n} takes Wakeshift's arguments`);
      assert.match(t.description!, /notifies you in this conversation/);
    }
    const plain = listed.find((x) => x.name === "plain")!;
    assert.ok(!("session_key" in (plain.inputSchema.properties as object)), "a tool the function leaves out is as it was");
    assert.ok(!asked.includes("subscribe_events"), "Wakeshift's own tools aren't asked about");
    const r = await client.callTool({ name: "run_crawler", arguments: { channel: "eng", session_key: "wsc_canary" } });
    assert.deepEqual(seen, [{ channel: "eng" }]);
    assert.match(text(r), /Woken by Monitor/);
    assert.equal(a.calls.filter((c) => c.path === "/event-subscriptions").length, 1);
    assert.throws(() => app().wake.attach(lowServer(sdk, [], async () => ({})).server, { wrap: "watch" as never }), /wrap is/);
    // A function that gives another client's event: logged once, served unwrapped, never thrown.
    const b = app();
    const other = app().message;
    const low2 = lowServer(sdk, [actor("x")], async () => ({ content: [{ type: "text", text: "ran" }] }));
    b.wake.attach(low2.server, { identify: () => ({ user: "ada" }), wrap: () => [other] });
    const c2 = await connect(sdk, low2.server);
    for (let i = 0; i < 2; i++) assert.ok(!("session_key" in ((await c2.listTools()).tools[0]!.inputSchema.properties as object)));
    assert.equal(text(await c2.callTool({ name: "run_x", arguments: {} })), "ran");
    assert.deepEqual(b.log.lines.filter((l) => l.fields.code === "wrap_bad_event").map((l) => [l.level, l.fields.tool]), [["error", "run_x"]]);
  });

  test(`${sdk} low-level Server: a tool in wrap whose own schema names session_key is served unwrapped, logged once (CR L4)`, async () => {
    const a = app();
    const legacy: Tool = { name: "legacy_watch", description: "Old.", inputSchema: { type: "object", properties: { session_key: { type: "string" } } } };
    const seen: Record<string, unknown>[] = [];
    const low = lowServer(sdk, [legacy], async (_n, args) => {
      seen.push(args);
      return { content: [{ type: "text", text: "ok" }] };
    });
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { legacy_watch: [a.message] } });
    const client = await connect(sdk, low.server);
    for (let i = 0; i < 2; i++) {
      const t = (await client.listTools()).tools.find((x) => x.name === "legacy_watch")!;
      assert.deepEqual(Object.keys(t.inputSchema.properties as object), ["session_key"], "nothing added");
      assert.equal(t.description, "Old.", "no sentence");
    }
    const r = await client.callTool({ name: "legacy_watch", arguments: { session_key: "theirs" } });
    assert.deepEqual(seen, [{ session_key: "theirs" }], "its own session_key reaches it");
    assert.equal(text(r), "ok", "no block");
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "wrap_arg_conflict").map((l) => l.fields), [{ code: "wrap_arg_conflict", tool: "legacy_watch" }]);
    assert.equal(a.log.lines.find((l) => l.fields.code === "wrap_arg_conflict")!.level, "error");
  });

  test(`${sdk} low-level Server: a wrap name the server never lists is logged once (CR L2)`, async () => {
    const a = app();
    const low = lowServer(sdk, [WATCH], async () => ({ content: [] }));
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { stat_crawl: [a.message] } });
    const client = await connect(sdk, low.server);
    await client.listTools();
    await client.listTools();
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "wrap_unknown_tool").map((l) => l.fields), [{ code: "wrap_unknown_tool", tool: "stat_crawl" }]);
  });

  test(`${sdk} low-level Server: the server's own tool named like a built-in loses to Wakeshift's, logged (CR L1)`, async () => {
    const a = app();
    const theirs: Tool = { name: "list_subscriptions", description: "Theirs.", inputSchema: { type: "object" } };
    const low = lowServer(sdk, [WATCH, theirs], async () => ({ content: [{ type: "text", text: "theirs" }] }));
    a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message] } });
    const client = await connect(sdk, low.server);
    const listed = (await client.listTools()).tools.filter((t) => t.name === "list_subscriptions");
    assert.equal(listed.length, 1);
    assert.notEqual(listed[0]!.description, "Theirs.");
    const r = await client.callTool({ name: "list_subscriptions", arguments: {} });
    assert.notEqual(text(r), "theirs", "Wakeshift's answers");
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "builtin_name_conflict").map((l) => l.fields), [{ code: "builtin_name_conflict", tool: "list_subscriptions" }]);
  });
}

test("McpServer: a vendor tool registered before attach under a built-in's name loses to Wakeshift's, logged (CR L1)", async () => {
  const a = app();
  const server = new V1McpServer({ name: "cards", version: "1" });
  server.registerTool("list_subscriptions", { description: "Theirs." }, async () => ({ content: [{ type: "text", text: "theirs" }] }));
  server.registerTool("watch", { description: "Watch a channel." }, async () => ({ content: [{ type: "text", text: "ok" }] }));
  a.wake.attach(server, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message] } });
  const client = await connect("1.x", server);
  const listed = (await client.listTools()).tools.filter((t) => t.name === "list_subscriptions");
  assert.equal(listed.length, 1);
  assert.notEqual(listed[0]!.description, "Theirs.");
  assert.notEqual(text(await client.callTool({ name: "list_subscriptions", arguments: {} })), "theirs");
  assert.deepEqual(a.log.codes("error"), ["builtin_name_conflict"]);
});

test("a second attach on the same server is refused, on a disabled client too (CR M1)", () => {
  const a = app();
  const server = new V1McpServer({ name: "cards", version: "1" });
  a.wake.attach(server);
  assert.throws(() => a.wake.attach(server), (e: unknown) => e instanceof UsageError && e.code === "config" && /attach/.test(e.message));
  const off = new Wakeshift({ key: "", logger: captureLog().logger });
  const other = new V1McpServer({ name: "cards", version: "1" });
  off.attach(other);
  assert.throws(() => off.attach(other), (e: unknown) => e instanceof UsageError && e.code === "config");
  // A server built per request gets its own attach.
  a.wake.attach(new V1McpServer({ name: "cards", version: "1" }));
});

test("an McpServer's inner server is refused: pass the McpServer (CR M3)", () => {
  const a = app();
  const server = new V1McpServer({ name: "cards", version: "1" });
  a.wake.attach(server);
  assert.throws(() => a.wake.attach(server.server as any), (e: unknown) => e instanceof UsageError && e.code === "config" && /McpServer/.test(e.message));
});

test("1.x without require() of ES modules: attach throws naming the Node floor (CR M4)", () => {
  const a = app();
  const features = process.features as unknown as Record<string, unknown>;
  const was = Object.getOwnPropertyDescriptor(features, "require_module");
  Object.defineProperty(features, "require_module", { value: false, configurable: true });
  try {
    assert.throws(
      () => a.wake.attach(new V1McpServer({ name: "cards", version: "1" })),
      (e: unknown) => e instanceof UsageError && e.code === "config" && /22\.12/.test(e.message),
    );
  } finally {
    if (was) Object.defineProperty(features, "require_module", was);
    else delete features.require_module;
  }
});

// fastmcp (punkpeye) hands out each session's low-level 1.x Server already
// connected to its transport, in its "connect" event, before the client's
// first request: attach takes it there, and initialize offers the events.
test("1.x low-level Server connected but not yet initialized: attach takes it, as fastmcp's connect event hands it out", async () => {
  const a = app();
  const s = new V1Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
  s.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [WATCH] as any }));
  s.setRequestHandler(CallToolRequestSchema, async (r) => {
    await a.wake.currentCall().subscribe(a.message, { channel: String(r.params.arguments?.channel) });
    return { content: [{ type: "text", text: "watching" }] };
  });
  // An event open to MCP Events clients, so attach advertises the capability.
  a.wake.defineEvent("chat.public", { description: "A public message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true });
  const [st, ct] = V1Memory.createLinkedPair();
  await s.connect(st);
  a.wake.attach(s as any, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message] } });
  const client = new V1Client({ name: "claude-code", version: "1" });
  await client.connect(ct);
  // What initialize answered with (1.x's client keeps only the keys it knows).
  assert.deepEqual((s as any).getCapabilities().events, { listChanged: false });
  assert.deepEqual((s as any).getCapabilities().tools, {}, "the server's own capabilities stay");
  const listed = (await client.listTools()).tools.find((t) => t.name === "watch")!;
  assert.ok("session_key" in (listed.inputSchema.properties as object));
  const r = await client.callTool({ name: "watch", arguments: { channel: "eng", session_key: "wsc_canary" } });
  assert.match(text(r), /Woken by Monitor/);
  // Once initialized, it's too late: the client has its capabilities.
  const late = new V1Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: {} } });
  const [lst, lct] = V1Memory.createLinkedPair();
  await late.connect(lst);
  await new V1Client({ name: "claude-code", version: "1" }).connect(lct);
  const b = app();
  b.wake.defineEvent("chat.public", { description: "A public message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true });
  assert.throws(() => b.wake.attach(late as any), /before connecting/);
});

test("1.x low-level Server already initialized (fastmcp's connect over stdio): attach tells the client its tools changed, if the server declares tools.listChanged", async () => {
  for (const listChanged of [true, false]) {
    const a = app();
    const s = new V1Server({ name: "cards", version: "1.0.0" }, { capabilities: { tools: listChanged ? { listChanged: true } : {} } });
    s.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [WATCH] as any }));
    s.setRequestHandler(CallToolRequestSchema, async () => ({ content: [{ type: "text", text: "ok" }] }));
    const [st, ct] = V1Memory.createLinkedPair();
    await s.connect(st);
    const client = new V1Client({ name: "claude-code", version: "1" });
    let changed = 0;
    client.setNotificationHandler(ToolListChangedNotificationSchema, () => void changed++);
    await client.connect(ct);
    const before = (await client.listTools()).tools.find((t) => t.name === "watch")!;
    assert.ok(!("session_key" in ((before.inputSchema.properties as object) ?? {})));
    a.wake.attach(s as any, { wrap: { watch: [a.message] } });
    await new Promise((r) => setTimeout(r, 10));
    assert.equal(changed, listChanged ? 1 : 0, `listChanged ${listChanged}`);
    const after = (await client.listTools()).tools.find((t) => t.name === "watch")!;
    assert.ok("session_key" in (after.inputSchema.properties as object), "attach still works late");
  }
});

// The same vendor pattern on an McpServer in a stateful session (stdio, an
// in-memory pair), with the vendor's tools/list wrapper set before attach
// and after: every list is amended once, and every call subscribes.
for (const order of ["before", "after"] as const) {
  test(`1.x McpServer, stateful session: a tools/list wrapper the vendor sets ${order} attach amends each list once, and calls keep subscribing`, async () => {
    const a = app();
    const server = new V1McpServer({ name: "cards", version: "1.0.0" });
    const wrapList = () => {
      const handlers: Map<string, (r: any, x: any) => Promise<any>> = (server.server as any)._requestHandlers;
      const old = handlers.get("tools/list")!;
      server.server.setRequestHandler(ListToolsRequestSchema, async (r: any, x: any) => {
        const out = await old(r, x);
        return { ...out, tools: out.tools.map((t: any) => ({ ...t, title: t.name.toUpperCase() })) };
      });
    };
    // McpServer sets tools/list on its first registerTool: register one first.
    server.registerTool("ping", { description: "Ping" }, async () => ({ content: [{ type: "text", text: "pong" }] }));
    if (order === "before") wrapList();
    const attached = a.wake.attach(server, { identify: () => ({ user: "ada" }) });
    attached.registerTool("watch", { description: "Watch a channel.", inputSchema: { channel: z.string() }, subscribes: [a.message] }, async ({ channel }, _extra, call) => {
      await call.subscribe(a.message, { channel });
      return "watching";
    });
    if (order === "after") wrapList();
    const [st, ct] = V1Memory.createLinkedPair();
    await server.connect(st);
    const client = new V1Client({ name: "claude-code", version: "1" });
    await client.connect(ct);
    for (let i = 0; i < 3; i++) {
      const listed = (await client.listTools()).tools.find((t) => t.name === "watch")!;
      assert.equal(listed.title, "WATCH");
      assert.equal(listed.description!.split("This tool is how").length, 2, `list ${i + 1}: the sentence once`);
      const r = await client.callTool({ name: "watch", arguments: { channel: `c${i}`, session_key: "wsc_canary" } });
      assert.match(text(r), /Woken by Monitor/, `call ${i + 1} subscribes`);
    }
    assert.equal(a.calls.filter((c) => c.path === "/event-subscriptions").length, 3);
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "wrap_arg_conflict"), []);
  });
}

test("a tool that subscribes whose description passes 2048 characters with the sentence is logged once (description_too_long)", async () => {
  const a = app();
  const long: Tool = { ...WATCH, description: "Watch a channel. " + "x".repeat(1900) };
  const low = lowServer("1.x", [long, { ...WATCH, name: "short" }], async () => ({ content: [] }));
  a.wake.attach(low.server, { identify: () => ({ user: "ada" }), wrap: { watch: [a.message], short: [a.message] } });
  const client = await connect("1.x", low.server);
  for (let i = 0; i < 2; i++) await client.listTools();
  assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "description_too_long").map((l) => [l.level, l.fields.tool]), [["warn", "watch"]]);
});
