// An app's MCP server on @modelcontextprotocol/server 2.x using the packed
// SDK, with @modelcontextprotocol/sdk 1.x not installed (scripts/pack-check.sh
// runs it with 2.x's oldest supported release and its latest). The core is
// faked.
import assert from "node:assert/strict";
import { McpServer, InMemoryTransport, createMcpHandler, type CallToolResult, type ServerContext } from "@modelcontextprotocol/server";
import { Client } from "@modelcontextprotocol/client";
import { z } from "zod";
import { Wakeshift, type ToolCall } from "@wakeshift/sdk";

const seed = Buffer.alloc(32, 7).toString("base64url");

// Wrapped tools: the handler's arguments are typed from attached.inputs (a
// zod object, as 2.x takes it), and a tool without Wakeshift's arguments
// doesn't compile. Only wrapped tools reach Wakeshift.
const wrapCalls: string[] = [];
const wrapBodies: { path: string; call?: unknown }[] = [];
const wrapFetch = (async (url: string | URL | Request, init?: RequestInit) => {
  const path = String(url).replace("http://core.test/api/apps/v1", "");
  wrapCalls.push(path);
  wrapBodies.push({ path, call: JSON.parse(String(init?.body ?? "{}")).call });
  const json =
    path === "/event-subscriptions"
      ? { event_subscription: { id: "es_1", user: "u1", created: true }, session: "wsc_1", pending: "pa_1", agent: { harness: "cursor", via: "client" } }
      : path === "/calls/addition"
        ? { text: "Wakeshift: woken." }
        : {};
  return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
}) as typeof fetch;
const wake = new Wakeshift({ key: `wst_test_ci-bot_${seed}`, core: "http://core.test", fetch: wrapFetch });
const turn = wake.defineEvent("game.turn", { description: "Your move", args: z.object({ table: z.string() }), payload: z.object({ toCall: z.number() }), text: (t) => `${t.toCall} to call`, personal: true });

function app(): McpServer {
  const server = new McpServer({ name: "cards", version: "1.0.0" });
  const attached = wake.attach(server, { identify: (ctx: ServerContext) => ({ user: ctx.http?.authInfo?.clientId ?? "u1" }), tools: { wait: { enabled: true } } });
  server.registerTool(
    "join",
    { inputSchema: attached.inputs(z.object({ table: z.string() })) },
    attached.events("join", async ({ table }, call: ToolCall<ServerContext>) => {
      const t: string = table;
      const sub = await call.subscribe(turn, { table: t }, { reason: "Playing" });
      return `Seated at ${table} (${sub.id}).`;
    }),
  );
  server.registerTool(
    "look",
    { inputSchema: attached.inputs(z.object({})), outputSchema: attached.output(z.object({ seats: z.number() })) },
    attached.events("look", () => ({ content: [{ type: "text" as const, text: "3" }], structuredContent: { seats: 3 } })),
  );
  server.registerTool("ping", { inputSchema: z.object({ table: z.string() }) }, async () => ({ content: [{ type: "text", text: "pong" }] }));
  const bare = attached.events("bare", () => "never");
  // @ts-expect-error: a wrapped tool's inputSchema comes from attached.inputs
  server.registerTool("bare", { inputSchema: z.object({ table: z.string() }) }, bare);
  return server;
}

// Over the 2025 handshake, in memory.
const [c, d] = InMemoryTransport.createLinkedPair();
const player = new Client({ name: "Cursor", version: "1.0.0" });
await Promise.all([app().connect(c), player.connect(d)]);
const joined = (await player.callTool({ name: "join", arguments: { table: "oak" } })) as CallToolResult;
assert.equal((joined.content[0] as { text: string }).text, "Seated at oak (es_1).\n\nWakeshift: woken.");
const looked = (await player.callTool({ name: "look", arguments: { session_key: "wsc_1" } })) as CallToolResult;
assert.deepEqual(looked.structuredContent, { seats: 3, wakeshift: "Wakeshift: woken." });
const { tools } = await player.listTools();
assert.deepEqual(Object.keys(tools.find((t) => t.name === "ping")!.inputSchema.properties ?? {}), ["table"], "a tool that isn't wrapped is left alone");
assert.deepEqual(
  wrapCalls.filter((p) => !p.startsWith("/manifest")),
  ["/event-subscriptions", "/calls/addition", "/calls/addition"],
);
assert.deepEqual(wrapBodies.find((b) => b.path === "/event-subscriptions")!.call, { client: "Cursor", user: "u1", tool: "join" });
await player.close();

// Over protocol 2026-07-28, as Claude Code connects: 2.x's createMcpHandler,
// a server per request; tools/list carries ttlMs and cacheScope, and the
// wait tool for a client that takes MCP tasks.
const handler = createMcpHandler(app, { onerror: () => {} });
const meta = {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "claude-code", version: "2.1.314" },
  "io.modelcontextprotocol/clientCapabilities": { extensions: { "io.modelcontextprotocol/tasks": {} } },
};
const post = async (method: string, params: Record<string, unknown>) => {
  const res = await handler.fetch(
    new Request("http://app.test/mcp", {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-protocol-version": "2026-07-28", "mcp-method": method },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params: { ...params, _meta: meta } }),
    }),
  );
  const text = await res.text();
  return JSON.parse(text.trim().startsWith("{") ? text : text.split("\n").find((l) => l.startsWith("data: "))!.slice(6));
};
const discover = await post("server/discover", {});
assert.ok(discover.result.capabilities.extensions["io.modelcontextprotocol/events"], "server/discover advertises MCP Events");
assert.ok(discover.result.capabilities.extensions["io.modelcontextprotocol/tasks"], "and the tasks extension");
const listed = await post("tools/list", {});
assert.equal(listed.result.resultType, "complete");
assert.equal(typeof listed.result.ttlMs, "number");
assert.equal(listed.result.cacheScope, "private");
assert.ok(listed.result.tools.some((t: { name: string }) => t.name === "wakeshift_wait"));
console.log("MCP SDK 2.x: ok");
