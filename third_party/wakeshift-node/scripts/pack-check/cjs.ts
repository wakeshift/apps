// A CommonJS app (no "type": "module") using the packed SDK through
// require(), as an older MCP server compiles: scripts/pack-check.sh builds it
// with tsc for module commonjs (node10 resolution, which reads typesVersions
// rather than exports) and for module nodenext, then runs it on Node. The
// imports below become require() calls; the types come from the package.
import assert from "node:assert/strict";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { CallToolRequestSchema, ListToolsRequestSchema, SUPPORTED_PROTOCOL_VERSIONS } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { Wakeshift, isWakeshiftError, redactSecrets, type ToolCall } from "@wakeshift/sdk";
import { attachTools, type ToolsContext } from "@wakeshift/sdk/tools";
import { attachMastra } from "@wakeshift/sdk/mastra";
import { createTestClient, createToolCall, withCall } from "@wakeshift/sdk/testing";

assert.equal(typeof require, "function", "this file runs as CommonJS");
const fetchFn = (async () => new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;
const wake = new Wakeshift({ key: "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV", core: "https://core.test", fetch: fetchFn, logLevel: "off" });

// A zod 3 schema renders through 1.x's converter, loaded without a top-level await.
const finished = wake.defineEvent("job.finished", {
  description: "A job finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string() }),
  text: (p, { job }) => `Job ${job} ${p.status}`,
});
const payload = (finished as unknown as { manifest: { payload: Record<string, unknown> } }).manifest.payload;
assert.equal(payload.type, "object");

// A low-level Server, attached, with its own handlers.
const server = new Server({ name: "jobs", version: "1.0.0" }, { capabilities: { tools: {} } });
server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [{ name: "start_job", inputSchema: { type: "object" as const, properties: {} } }] }));
server.setRequestHandler(CallToolRequestSchema, async () => {
  const call: ToolCall = wake.currentCall();
  await call.subscribe(finished, { job: "j1" });
  return { content: [{ type: "text" as const, text: "started" }] };
});
wake.attach(server, { wrap: { start_job: [finished] } });
// attach taught the CommonJS copy of 1.x protocol 2026-07-28, the copy this app's transport uses.
assert.ok(SUPPORTED_PROTOCOL_VERSIONS.includes("2026-07-28"), SUPPORTED_PROTOCOL_VERSIONS.join(", "));
assert.equal(redactSecrets("wsc_fakefakefakefakefakefakefa"), "[redacted]");

// The subpaths load, with their types.
const other = new Wakeshift({ key: "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV", core: "https://core.test", fetch: fetchFn, logLevel: "off" });
other.defineEvent("x.y", { description: "X", args: z.object({ a: z.string() }), payload: z.object({}), public: true });
const tools = attachTools(other);
const ctx: ToolsContext = { meta: {} };
void ctx;
assert.ok(tools.builtins().some((t) => t.metadata.name === "subscribe_events"));
assert.equal(typeof attachMastra, "function");
assert.equal(isWakeshiftError(new Error("x")), false);
// @ts-expect-error: types are checked, not any
void wake.notACall;
console.log("CommonJS require: ok");

// @wakeshift/sdk/testing loads under require() too.
const testWake = createTestClient({ logLevel: "off" });
assert.equal(testWake.enabled, true);
assert.equal(typeof createToolCall, "function");
assert.equal(typeof withCall, "function");
