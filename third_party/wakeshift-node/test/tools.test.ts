// @wakeshift/sdk/tools: Wakeshift for a runtime with no MCP server. list
// and call for one that answers JSON-RPC; tool and builtins for a framework
// that takes tool definitions (xmcp). Both scope Wakeshift's arguments as
// tools/list does everywhere (subscribing-tools-list.json compact rules).
import { test } from "node:test";
import assert from "node:assert/strict";
import { z } from "zod";
import { Wakeshift } from "../src/index.js";
import { attachTools } from "../src/tools.js";
import { captureLog } from "./logs.js";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const WAKE = ["conversation_id", "harness", "session_key"];

function app() {
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const json =
      path === "/event-subscriptions"
        ? { event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" }
        : path === "/calls/addition"
          ? { text: "Woken." }
          : path === "/app"
            ? { id: "a", name: "App" }
            : {};
    return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: captureLog().logger });
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({}), public: true });
  return { wake, crawl, calls };
}

test("tools.tool and builtins give the schemas tools/list lists: all three wake args when it subscribes, only session_key with subscribes [] and on list_subscriptions", () => {
  const { wake, crawl } = app();
  const tools = attachTools(wake, { identify: () => ({ user: "u1" }) });
  const watch = tools.tool("watch", { description: "Watch a crawl.", inputSchema: { job: z.string() }, subscribes: [crawl] }, async () => "ok");
  const peek = tools.tool("peek", { description: "Peek.", inputSchema: { job: z.string() }, subscribes: [] }, async () => "ok");
  assert.deepEqual(Object.keys(watch.schema).sort(), ["job", ...WAKE].sort());
  assert.deepEqual(Object.keys(peek.schema).sort(), ["job", "session_key"]);
  assert.ok(watch.metadata.description.startsWith("Watch a crawl."));
  assert.notEqual(watch.metadata.description, "Watch a crawl.", "the subscribes sentence is added");
  const b = Object.fromEntries(tools.builtins().map((t) => [t.metadata.name, Object.keys(t.schema)]));
  assert.deepEqual(Object.keys(b).sort(), ["list_subscriptions", "subscribe_events", "unsubscribe_events"]);
  for (const k of WAKE) assert.ok(b.subscribe_events!.includes(k), `subscribe_events has ${k}`);
  assert.deepEqual(b.unsubscribe_events!.filter((k) => WAKE.includes(k)), ["session_key"]);
  assert.deepEqual(b.list_subscriptions!.filter((k) => WAKE.includes(k)), ["session_key"]);
});

test("tools.tool's handler strips Wakeshift's arguments, subscribes, and answers with the block", async () => {
  const { wake, crawl, calls } = app();
  const tools = attachTools(wake, { identify: () => ({ user: "u1" }) });
  let seen: unknown;
  const watch = tools.tool("watch", { description: "Watch a crawl.", inputSchema: { job: z.string() }, subscribes: [crawl] }, async (args, _ctx, call) => {
    seen = args;
    const sub = await call.subscribe(crawl, { job: args.job });
    return `watching (${sub.state})`;
  });
  const out = await watch.handler({ job: "j1", harness: "claude-code" }, { clientInfo: { name: "claude-code", version: "2" } });
  assert.deepEqual(seen, { job: "j1" });
  assert.ok(calls.some((c) => c.path === "/event-subscriptions"));
  const text = (out.content as { type: string; text: string }[]).map((c) => c.text).join("\n");
  assert.ok(text.includes("watching (created)"), text);
  assert.ok(text.includes("Woken."), text);
});

test("tools.list amends a catalog with the same scoping, and call strips the arguments of a tool that doesn't subscribe", async () => {
  const { wake, crawl } = app();
  const tools = attachTools(wake, { identify: () => ({ user: "u1" }), wrap: { watch: [crawl] } });
  const catalog = [
    { name: "watch", description: "Watch a crawl.", inputSchema: { type: "object", properties: { job: { type: "string" } } } },
    { name: "other", description: "Other.", inputSchema: { type: "object", properties: { x: { type: "string" } } } },
  ];
  const listed = Object.fromEntries((await tools.list(catalog)).map((t) => [t.name, Object.keys((t.inputSchema as any)?.properties ?? {})]));
  assert.deepEqual(listed.watch!.sort(), ["job", ...WAKE].sort());
  assert.deepEqual(listed.other, ["x"]);
  assert.deepEqual(listed.unsubscribe_events!.filter((k) => WAKE.includes(k)), ["session_key"]);
  assert.deepEqual(listed.list_subscriptions!.filter((k) => WAKE.includes(k)), ["session_key"]);
  let got: unknown;
  await tools.call({ name: "other", arguments: { x: "1", session_key: "wsc_x", harness: "codex" } }, {}, async (p) => {
    got = p.arguments;
    return { content: [] };
  });
  assert.deepEqual(got, { x: "1" });
});

// A hand-rolled JSON-RPC runtime (a proxy, a Worker) answering protocol
// 2026-07-28 with attachTools, as the official 2.x client sees it: the
// built-ins' results carry resultType, and a throw from dispatch reaches
// the runtime unchanged, to answer as it likes.
test("attachTools on 2026-07-28: the 2.x client takes the built-ins' results, and dispatch's throws reach the runtime", async () => {
  const { Client, StreamableHTTPClientTransport } = await import("@modelcontextprotocol/client");
  const { createServer } = await import("node:http");
  const { wake, crawl } = app();
  const tools = attachTools(wake, { identify: () => ({ user: "u1" }), wrap: { watch: [crawl] } });
  const catalog = [
    { name: "watch", description: "Watch a crawl.", inputSchema: { type: "object", properties: { job: { type: "string" } } } },
    { name: "fails", description: "Fails.", inputSchema: { type: "object", properties: {} } },
  ];
  class Refused extends Error {
    rpc = { code: -32001, message: "your plan doesn't include this tool" };
  }
  const thrown: unknown[] = [];
  const http = createServer(async (req, res) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    const msg = JSON.parse(body);
    const ctx = { meta: msg.params?._meta, headers: req.headers as Record<string, string> };
    const reply = (x: Record<string, unknown>) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, ...x }));
    };
    if (msg.id === undefined) return void res.writeHead(202).end();
    try {
      switch (msg.method) {
        case "server/discover":
          return reply({
            result: { resultType: "complete", supportedVersions: ["2026-07-28"], capabilities: { tools: {} }, ttlMs: 0, cacheScope: "public", _meta: { "io.modelcontextprotocol/serverInfo": { name: "proxy", version: "1" } } },
          });
        case "tools/list":
          return reply({ result: { resultType: "complete", ttlMs: 0, cacheScope: "private", tools: await tools.list(catalog, ctx) } });
        case "tools/call":
          return reply({
            result: await tools.call(msg.params, ctx, async (p) => {
              if (p.name === "fails") throw new Refused("refused");
              return { content: [{ type: "text", text: `ran ${p.name}` }] };
            }),
          });
      }
      reply({ error: { code: -32601, message: "Method not found" } });
    } catch (err) {
      thrown.push(err);
      reply({ error: err instanceof Refused ? err.rpc : { code: -32603, message: "internal" } });
    }
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const port = (http.address() as { port: number }).port;
  const client = new Client({ name: "claude-code", version: "2.1.314" }, { versionNegotiation: { mode: { pin: "2026-07-28" } } });
  try {
    await client.connect(new StreamableHTTPClientTransport(new URL(`http://127.0.0.1:${port}/mcp`)));
    const listed = (await client.listTools()).tools;
    assert.ok(listed.some((t) => t.name === "list_subscriptions"));
    const r = await client.callTool({ name: "list_subscriptions", arguments: {} });
    assert.ok(Array.isArray(r.content), "the 2.x client took the built-in's result");
    await assert.rejects(client.callTool({ name: "fails", arguments: {} }), /plan doesn't include/);
    assert.equal(thrown.length, 1);
    assert.ok(thrown[0] instanceof Refused, "dispatch's own error, unchanged");
  } finally {
    await client.close().catch(() => {});
    http.close();
  }
});
