// An app's MCP server on @modelcontextprotocol/sdk 1.x using the packed SDK
// with whichever zod it installed (scripts/pack-check.sh runs it with zod 3
// and zod 4). The core is faked.
import assert from "node:assert/strict";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const quiet = { error: () => {}, warn: () => {}, info: () => {} };
const fakeFetch = (async () => new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;

// Events: the zod schemas become JSON Schema with whichever zod is installed.
const ev = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fakeFetch, logger: quiet });
const build = ev.defineEvent("ci.build", {
  description: "A CI run finished",
  args: z.object({ repo: z.string(), branch: z.string().optional() }),
  payload: z.object({ conclusion: z.string(), branch: z.string() }),
  text: (p, a) => `CI ${p.conclusion} on ${a.repo}`,
});
// The manifest is internal, so the published types leave it out: read it at run time.
const manifest = (build as unknown as { manifest: { topic: string; filter?: Record<string, string>; arguments: { required?: string[] }; subscribe: string } }).manifest;
assert.equal(manifest.topic, "ev/ci.build/{repo}");
assert.deepEqual(manifest.filter, { branch: "{branch}" });
assert.deepEqual(manifest.arguments.required, ["repo"]);
assert.equal(manifest.subscribe, "tools");
// Internal members are stripped from the published types.
// @ts-expect-error requestApp is internal
void ev.requestApp;
// @ts-expect-error event.subscribe is call.subscribe in a tool that subscribes
void build.subscribe;

// Tools that subscribe: the handler's arguments are typed from the tool's
// own inputSchema; only tools that subscribe reach Wakeshift.
const wrapCalls: string[] = [];
const wrapBodies: { path: string; call?: unknown }[] = [];
const wrapFetch = (async (url: string | URL | Request, init?: RequestInit) => {
  const path = String(url).replace("https://core.test/api/apps/v1", "");
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
const wr = new Wakeshift({ key: KEY, core: "https://core.test", fetch: wrapFetch, logger: quiet });
const turn = wr.defineEvent("game.turn", { description: "Your move", args: z.object({ table: z.string() }), payload: z.object({ toCall: z.number() }), text: (t) => `${t.toCall} to call`, personal: true });
const wrServer = new McpServer({ name: "cards", version: "1.0.0" });
const attached = wr.attach(wrServer, { identify: () => ({ user: "u1" }) });
attached.registerTool("join", { inputSchema: { table: z.string() }, subscribes: [turn] }, async ({ table }, _ctx, call) => {
  const t: string = table;
  const sub = await call.subscribe(turn, { table: t }, { reason: "Playing" });
  return `Seated at ${table} (${sub.state === "created" ? sub.id : sub.state}).`;
});
attached.registerTool("look", { inputSchema: {}, outputSchema: { seats: z.number() }, subscribes: [] }, () => ({ content: [{ type: "text" as const, text: "3" }], structuredContent: { seats: 3 } }));
wrServer.registerTool("ping", { inputSchema: { table: z.string() } }, async () => ({ content: [{ type: "text", text: "pong" }] }));
const [c, d] = InMemoryTransport.createLinkedPair();
const player = new Client({ name: "Cursor", version: "1.0.0" });
await Promise.all([wrServer.connect(c), player.connect(d)]);
const joined = (await player.callTool({ name: "join", arguments: { table: "oak" } })) as CallToolResult;
assert.equal((joined.content[0] as { text: string }).text, "Seated at oak (es_1).\n\nWakeshift: woken.");
const looked = (await player.callTool({ name: "look", arguments: { session_key: "wsc_1" } })) as CallToolResult;
assert.deepEqual(looked.structuredContent, { seats: 3, wakeshift: "Wakeshift: woken." });
const { tools } = await player.listTools();
assert.deepEqual(Object.keys(tools.find((t) => t.name === "ping")!.inputSchema.properties ?? {}), ["table"], "a tool that doesn't subscribe is left alone");
assert.deepEqual(Object.keys(tools.find((t) => t.name === "join")!.inputSchema.properties ?? {}).sort(), ["conversation_id", "harness", "session_key", "table"]);
const pinged = (await player.callTool({ name: "ping", arguments: { table: "oak", session_key: "wsc_1" } })) as CallToolResult;
assert.deepEqual(pinged.content, [{ type: "text", text: "pong" }]);
assert.deepEqual(
  wrapCalls.filter((p) => !p.startsWith("/manifest") && p !== "/app"),
  ["/event-subscriptions", "/calls/addition", "/calls/addition"],
);
assert.deepEqual(wrapBodies.find((b) => b.path === "/event-subscriptions")!.call, { client: "Cursor", user: "u1", tool: "join" });
await player.close();
console.log("MCP SDK 1.x: ok");
