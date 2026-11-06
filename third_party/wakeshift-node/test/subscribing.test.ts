// Tools that subscribe (design §2.4, §8 PR 5a): registered through the
// attachment or named in wrap, they give the same tools/list and the same
// call (subscribing-tools-list.json). Wakeshift works at the protocol
// layer, so a vendor's middleware, a Proxy around registerTool or the
// legacy server.tool can't break it (#37, CR H3), wake.currentCall(ctx)
// finds the call without AsyncLocalStorage (CR M2), and a tool that doesn't
// subscribe never sees session_key (SEC M3).
import { test } from "node:test";
import assert from "node:assert/strict";
import { AsyncResource } from "node:async_hooks";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport, MCP } from "./mcp.js";
import { Wakeshift, isWakeshiftError } from "../src/index.js";
import { captureLog } from "./logs.js";

const fixture = JSON.parse(readFileSync(new URL("../../test/subscribing-tools-list.json", import.meta.url), "utf8"));
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const WOKEN = "Woken by Monitor.";

function app(enabled = true, only?: string[]) {
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const reply = (json: unknown) => new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/app":
        return reply({ id: "crawler", name: fixture.app, key: { key_id: "k1", name: "test" } });
      case "/event-subscriptions":
        return reply({ event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" });
      case "/calls/addition":
        return reply({ text: WOKEN });
    }
    return reply(body ?? {});
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: enabled ? KEY : "", core: "https://core.test", fetch: fetchFn, logger: log.logger });
  // The fixture's events, with access as it says (who may subscribe comes in PR 6: authorize allows all).
  const events: Record<string, any> = {};
  for (const [name, e] of Object.entries<any>(fixture.events)) {
    if (only && !only.includes(name)) continue;
    const args = Object.fromEntries(Object.entries<any>(e.args.properties).map(([k, p]) => [k, z.string().describe(p.description)]));
    events[name] = wake.defineEvent(name, {
      description: e.description,
      args: z.object(args),
      payload: z.object({ status: z.string() }).passthrough(),
      text: (p: any) => p.status,
      ...(e.access === "authorize" ? { authorize: () => true } : {}),
    });
  }
  return { wake, events, calls, log };
}

// zodShape is a fixture's JSON Schema (string properties only) as a zod shape.
function zodShape(schema: any): Record<string, z.ZodTypeAny> {
  return Object.fromEntries(Object.entries<any>(schema.properties).map(([k, p]) => [k, (schema.required ?? []).includes(k) ? z.string().describe(p.description) : z.string().optional().describe(p.description)]));
}

async function connect(server: McpServer, name = "claude-code"): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

// serve registers a scenario's tools: through the attachment, or with the host SDK and wrap.
function serve(a: ReturnType<typeof app>, scenario: any, how: "register" | "wrap"): McpServer {
  const server = new McpServer({ name: "crawler", version: "1" });
  const tools = Object.fromEntries(scenario.tools.map((n: string) => [n, fixture.vendor_tools[n]]));
  const wrap: Record<string, any[]> = {};
  for (const [name, t] of Object.entries<any>(tools)) if (how === "wrap" && t.subscribes) wrap[name] = t.subscribes.map((e: string) => a.events[e]);
  const attached = a.wake.attach(server, { identify: () => ({ user: "ada" }), wrap, builtinTools: scenario.builtin_tools ?? undefined });
  for (const [name, t] of Object.entries<any>(tools)) {
    const config: any = { description: t.description, inputSchema: zodShape(t.inputSchema) };
    if (t.outputSchema) config.outputSchema = zodShape(t.outputSchema);
    const result = t.outputSchema ? { content: [], structuredContent: { job: "j1" } } : { content: [{ type: "text", text: "ok" }] };
    if (how === "register" && t.subscribes) {
      attached.registerTool(name, { ...config, subscribes: t.subscribes.map((e: string) => a.events[e]) }, async () => result as any);
    } else {
      server.registerTool(name, config, async () => result as any);
    }
  }
  return server;
}

// shape is a listed tool as the fixtures compare it (README: shape normalization).
function shapeOf(schema: any): any {
  if (!schema) return undefined;
  const out: any = { type: schema.type };
  if (schema.properties && Object.keys(schema.properties).length > 0) {
    out.properties = Object.fromEntries(
      Object.entries<any>(schema.properties).map(([k, p]) => [
        k,
        { ...Object.fromEntries(["type", "description", "enum"].filter((f) => p[f] !== undefined).map((f) => [f, p[f]])), ...(p.items?.type ? { items: { type: p.items.type } } : {}) },
      ]),
    );
  }
  if (schema.required?.length) out.required = [...schema.required].sort();
  if (out.properties) for (const p of Object.values<any>(out.properties)) if (p.type === "object" && schema.properties) delete p.properties;
  return out;
}

const listed = async (server: McpServer) => {
  const tools = (await (await connect(server)).listTools()).tools;
  return Object.fromEntries(tools.map((t: any) => [t.name, { description: t.description, inputSchema: shapeOf(t.inputSchema), outputSchema: shapeOf(t.outputSchema), annotations: t.annotations }]));
};

test("registerTool and wrap serve exactly the fixture's tools/list, for every scenario (subscribing-tools-list.json)", async () => {
  for (const scenario of fixture.scenarios) {
    for (const how of ["register", "wrap"] as const) {
      const got = await listed(serve(app(scenario.enabled, scenario.events), scenario, how));
      assert.deepEqual(Object.keys(got).sort(), scenario.tools_list.map((t: any) => t.name).sort(), `${scenario.name} (${how})`);
      for (const want of scenario.tools_list) {
        const g = got[want.name];
        assert.deepEqual(
          { description: g.description, inputSchema: g.inputSchema, outputSchema: g.outputSchema, annotations: g.annotations },
          { description: want.description, inputSchema: want.inputSchema, outputSchema: want.outputSchema, annotations: want.annotations },
          `${scenario.name} (${how}): ${want.name}`,
        );
      }
    }
  }
});

test("the compact strings come from one place, the fixture's compact_strings", async () => {
  const { COMPACT } = await import("../src/wording.js");
  const want = Object.fromEntries(Object.entries(fixture.compact_strings).filter(([k]) => !k.startsWith("_")));
  assert.deepEqual({ ...COMPACT }, want);
});

test("a disabled client serves the vendor's tools exactly as written (subscribing-tools-list.json)", async () => {
  const scenario = fixture.scenarios.find((s: any) => !s.enabled);
  for (const how of ["register", "wrap"] as const) {
    const got = await listed(serve(app(false), scenario, how));
    assert.deepEqual(Object.keys(got).sort(), scenario.tools_list.map((t: any) => t.name).sort(), how);
    for (const want of scenario.tools_list) {
      assert.deepEqual(
        { description: got[want.name].description, inputSchema: got[want.name].inputSchema, outputSchema: got[want.name].outputSchema },
        { description: want.description, inputSchema: want.inputSchema, outputSchema: want.outputSchema },
        `${how}: ${want.name}`,
      );
    }
  }
});

// A vendor's logging middleware around every tool callback (BrowserStack's
// shape): it re-wraps the callback and logs the arguments it gets.
function withLoggingMiddleware(server: McpServer, seen: unknown[]): void {
  const register = server.registerTool.bind(server) as (...a: any[]) => any;
  (server as any).registerTool = (name: string, config: unknown, cb: (...a: unknown[]) => unknown) =>
    register(name, config, (...args: unknown[]) => {
      seen.push(JSON.parse(JSON.stringify(args[0])));
      return cb(...args);
    });
}

test("vendor middleware that re-wraps the callback can't break a tool that subscribes, and never sees session_key (#37)", async () => {
  const a = app();
  const server = new McpServer({ name: "crawler", version: "1" });
  const seen: unknown[] = [];
  withLoggingMiddleware(server, seen);
  const attached = a.wake.attach(server, { identify: () => ({ user: "ada" }) });
  attached.registerTool("start_crawl", { inputSchema: { url: z.string() }, subscribes: [a.events["crawl.finished"]] }, async ({ url }, _ctx, call) => {
    await call.subscribe(a.events["crawl.finished"], { job: "j1" });
    return `Crawling ${url}.`;
  });
  const client = await connect(server);
  const r: any = await client.callTool({ name: "start_crawl", arguments: { url: "https://x", session_key: "wsc_canary", harness: "codex" } });
  assert.deepEqual(seen, [{ url: "https://x" }]);
  assert.equal(r.content[0].text, `Crawling https://x.\n\n${WOKEN}`);
  assert.equal(a.calls.find((c) => c.path === "/event-subscriptions")!.body.call.session, "wsc_canary");
});

test("a Proxy around registerTool, and the legacy server.tool named in wrap, still subscribe (#37)", async () => {
  const a = app();
  const server = new McpServer({ name: "crawler", version: "1" });
  (server as any).registerTool = new Proxy(server.registerTool, {
    apply(target, self, args: any[]) {
      const cb = args[2];
      args[2] = (...x: unknown[]) => cb(...x);
      return Reflect.apply(target, self, args);
    },
  });
  const attached = a.wake.attach(server, { identify: () => ({ user: "ada" }), wrap: { legacy: [a.events["crawl.finished"]] } });
  attached.registerTool("proxied", { inputSchema: { job: z.string() }, subscribes: [a.events["crawl.finished"]] }, async ({ job }, _ctx, call) => {
    await call.subscribe(a.events["crawl.finished"], { job });
    return "ok";
  });
  if (MCP === 1) {
    (server as any).tool("legacy", { job: z.string() }, async ({ job }: { job: string }) => {
      await a.wake.currentCall().subscribe(a.events["crawl.finished"], { job });
      return { content: [{ type: "text", text: "ok" }] };
    });
  } else {
    server.registerTool("legacy", { inputSchema: z.object({ job: z.string() }) }, async ({ job }: { job: string }) => {
      await a.wake.currentCall().subscribe(a.events["crawl.finished"], { job });
      return { content: [{ type: "text", text: "ok" }] };
    });
  }
  const client = await connect(server);
  for (const name of ["proxied", "legacy"]) {
    const r: any = await client.callTool({ name, arguments: { job: "j1", session_key: "wsc_s1" } });
    assert.equal(r.isError, undefined, `${name}: ${JSON.stringify(r)}`);
    assert.match(r.content.map((c: any) => c.text).join("\n"), new RegExp(WOKEN), name);
  }
  assert.equal(a.calls.filter((c) => c.path === "/event-subscriptions").length, 2);
});

test("wake.currentCall(ctx) finds the call on an McpServer from code that lost the async context (CR M2)", async () => {
  const a = app();
  const server = new McpServer({ name: "crawler", version: "1" });
  // A resource made outside any call: code run in it has no call in AsyncLocalStorage.
  const outside = new AsyncResource("outside");
  const attached = a.wake.attach(server, { identify: () => ({ user: "ada" }) });
  let lost: unknown;
  attached.registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [a.events["crawl.finished"]] }, async ({ job }, ctx) => {
    lost = outside.runInAsyncScope(() => {
      try {
        return a.wake.currentCall();
      } catch (e) {
        return e;
      }
    });
    const call = outside.runInAsyncScope(() => a.wake.currentCall(ctx));
    await call.subscribe(a.events["crawl.finished"], { job });
    return "ok";
  });
  const client = await connect(server);
  const r: any = await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  assert.equal(r.isError, undefined, JSON.stringify(r));
  assert.ok(isWakeshiftError(lost, "no_call"), "without ctx, outside the async context: no call");
  assert.equal(a.calls.filter((c) => c.path === "/event-subscriptions").length, 1);
});

test("a tool that doesn't subscribe never sees Wakeshift's arguments, even with a strict schema (SEC M3)", async () => {
  const a = app();
  const server = new McpServer({ name: "crawler", version: "1" });
  a.wake.attach(server, { identify: () => ({ user: "ada" }) });
  const seen: unknown[] = [];
  server.registerTool("get_page", { inputSchema: z.strictObject({ url: z.string() }) }, async (args: any) => {
    seen.push(args);
    return { content: [{ type: "text", text: "page" }] };
  });
  const client = await connect(server);
  const r: any = await client.callTool({ name: "get_page", arguments: { url: "https://x", session_key: "wsc_canary", harness: "codex", conversation_id: "c1" } });
  assert.equal(r.isError, undefined, JSON.stringify(r));
  assert.deepEqual(seen, [{ url: "https://x" }]);
  assert.equal(r.content.length, 1, "no block");
  assert.deepEqual(a.calls, [], "Wakeshift wasn't asked");
});

test("a disabled client's registerTool registers the tool as written, its handler getting a disabled call", async () => {
  const a = app(false);
  const server = new McpServer({ name: "crawler", version: "1" });
  const attached = a.wake.attach(server);
  let state: string | undefined;
  attached.registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [a.events["crawl.finished"]] }, async ({ job }, _ctx, call) => {
    state = (await call.subscribe(a.events["crawl.finished"], { job })).state;
    await assert.rejects((call as any).subscribe(a.events["build.finished"], { repo: "x" }), (e: unknown) => isWakeshiftError(e, "undeclared_subscribe"));
    return "ok";
  });
  const client = await connect(server);
  const r: any = await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  assert.deepEqual(r.content, [{ type: "text", text: "ok" }]);
  assert.equal(state, "disabled");
  assert.deepEqual(a.calls, []);
});
