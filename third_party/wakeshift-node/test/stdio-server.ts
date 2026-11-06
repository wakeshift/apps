// A stdio MCP server with Wakeshift attached, as a vendor writes one, for
// stdio.test.ts: run as its own process, with or without WAKESHIFT_KEY, and
// nothing but JSON-RPC may reach its stdout. The default logger (no logger
// option) is the one under test. WAKESHIFT_TEST_MCP=v2 serves it on
// @modelcontextprotocol/server 2.x.
import { z } from "zod";
import * as v1stdio from "@modelcontextprotocol/sdk/server/stdio.js";
import * as v2stdio from "@modelcontextprotocol/server/stdio";
import { McpServer, MCP } from "./mcp.js";
import { Wakeshift } from "../src/index.js";

const wake = new Wakeshift();
const ping = wake.defineEvent("room.ping", { description: "A ping in a room", args: z.object({ room: z.string() }), payload: z.object({ text: z.string() }), public: true });
const server = new McpServer({ name: "stdio", version: "1.0.0" });
const attached = wake.attach(server);
attached.registerTool("watch", { description: "Watch a room", inputSchema: { room: z.string() }, subscribes: [ping] }, async ({ room }, _extra, call) => {
  await call.subscribe(ping, { room });
  return `Watching ${room}.`;
});
server.registerTool("hello", { description: "Say hello" }, async () => ({ content: [{ type: "text", text: "hello" }] }));
// WAKESHIFT_TEST_WATCH: a watch that never finishes runs from the start, and
// says how it ended on stderr: the process must still exit when stdin ends.
if (process.env.WAKESHIFT_TEST_WATCH) {
  const w = ping.watch({ room: "oak" }, async () => null, { every: "1h", timeout: "2h" });
  void w.done.then((r) => process.stderr.write(`watch-ended:${r.outcome}\n`));
}
const transport = MCP === 2 ? new v2stdio.StdioServerTransport() : new v1stdio.StdioServerTransport();
await server.connect(transport as never);
