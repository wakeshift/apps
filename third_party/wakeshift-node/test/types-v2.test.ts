// What the compiler infers for wrapped tools on @modelcontextprotocol/server
// 2.x (types.test.ts does the same for @modelcontextprotocol/sdk 1.x):
// checked by tsc (npm test compiles this file). Nothing here runs against a
// core.
import { test } from "node:test";
import { expectTypeOf } from "expect-type";
import { McpServer, type CallToolResult, type ServerContext } from "@modelcontextprotocol/server";
import { z } from "zod";
import { Wakeshift, type Attached, type Subscription, type ToolCall } from "../src/index.js";

const seed = Buffer.alloc(32, 9).toString("base64url");

function setup() {
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "http://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const turn = wake.defineEvent("game.turn", {
    description: "Your move",
    args: z.object({ table: z.string() }),
    payload: z.object({ toCall: z.number() }),
    text: (t) => `${t.toCall} to call`,
    personal: true,
  });
  const server = new McpServer({ name: "t", version: "1" });
  // identify gets 2.x's own context: the token in ctx.http.authInfo.
  const attached = wake.attach(server, {
    identify: (ctx) => {
      expectTypeOf(ctx).toEqualTypeOf<ServerContext>();
      return { user: ctx.http?.authInfo?.clientId };
    },
    events: { native: (ctx) => ctx.mcpReq.method === "tools/call" && false },
  });
  return { wake, turn, server, attached };
}

test("wrapped tools' types on the MCP SDK 2.x", () => {
  const { turn, server, attached } = setup();
  expectTypeOf(attached).toEqualTypeOf<Attached<ServerContext>>();

  // A zod object, as 2.x takes it: the handler's arguments are the tool's own.
  server.registerTool(
    "join",
    { description: "Join a table", inputSchema: attached.inputs(z.object({ table: z.string(), seats: z.number().optional() })) },
    attached.events("join", async (args, call) => {
      expectTypeOf(args).toEqualTypeOf<{ table: string; seats?: number | undefined }>();
      expectTypeOf(call).toEqualTypeOf<ToolCall<ServerContext>>();
      expectTypeOf(call.extra).toEqualTypeOf<ServerContext>();
      const sub = await call.subscribe(turn, { table: args.table }, { reason: "Playing" });
      expectTypeOf(sub).toEqualTypeOf<Subscription>();
      // @ts-expect-error: the event's arguments are checked
      await call.subscribe(turn, { tabel: "x" });
      // @ts-expect-error: Wakeshift's arguments aren't the handler's
      void args.session_key;
      return `Seated at ${args.table}.`;
    }),
  );

  // Typed output, with the opt-in: 2.x's own CallToolResult is a result.
  server.registerTool(
    "look",
    { inputSchema: attached.inputs(z.object({ table: z.string() })), outputSchema: attached.output(z.object({ seats: z.number() })) },
    attached.events("look", ({ table }) => {
      expectTypeOf(table).toEqualTypeOf<string>();
      const out: CallToolResult = { content: [{ type: "text", text: "3" }], structuredContent: { seats: 3 } };
      return out;
    }),
  );

  // A bare shape works too (2.x's deprecated form).
  server.registerTool(
    "count",
    { inputSchema: attached.inputs({ n: z.number() }) },
    attached.events("count", ({ n }) => {
      expectTypeOf(n).toEqualTypeOf<number>();
      return "ok";
    }),
  );

  // A tool whose inputSchema lacks Wakeshift's arguments doesn't compile.
  server.registerTool(
    "bare",
    // @ts-expect-error: the input schema must come from attached.inputs (attached.events needs its three keys)
    { inputSchema: z.object({ table: z.string() }) },
    attached.events("bare", () => "x"),
  );

  // Nor does a handler returning something that isn't a result.
  server.registerTool(
    "wrong",
    { inputSchema: attached.inputs(z.object({})) },
    // @ts-expect-error: a string or a CallToolResult
    attached.events("wrong", () => 42),
  );
});

test("subscribes: call.subscribe takes only the declared events, and a tool that declares none has none", () => {
  const { wake, turn, server, attached } = setup();
  const news = wake.defineEvent("game.news", { description: "News", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), text: (n) => n.text });
  expectTypeOf(turn.name).toEqualTypeOf<"game.turn">();

  server.registerTool(
    "join",
    { inputSchema: attached.inputs(z.object({ table: z.string() })) },
    attached.events("join", { subscribes: [turn] }, async ({ table }, call) => {
      expectTypeOf(call).toEqualTypeOf<ToolCall<ServerContext, typeof turn>>();
      const sub = await call.subscribe(turn, { table }, { reason: "Playing" });
      expectTypeOf(sub).toEqualTypeOf<Subscription>();
      // @ts-expect-error: the event's arguments are checked
      await call.subscribe(turn, { tabel: "x" });
      // @ts-expect-error: game.news isn't declared
      await call.subscribe(news, { table });
      await call.unsubscribe(news, { table });
      return "ok";
    }),
  );

  server.registerTool(
    "both",
    { inputSchema: attached.inputs(z.object({ table: z.string() })) },
    attached.events("both", { subscribes: [turn, news] }, async ({ table }, call) => {
      await call.subscribe(turn, { table });
      await call.subscribe(news, { table });
      return "ok";
    }),
  );

  server.registerTool(
    "look",
    { inputSchema: attached.inputs(z.object({ table: z.string() })) },
    attached.events("look", { subscribes: [] }, async ({ table }, call) => {
      // @ts-expect-error: a tool that declares no events has no subscribe
      await call.subscribe(turn, { table });
      await call.resolveSession();
      return "ok";
    }),
  );

  // The two-argument form keeps a call that takes any of the app's events (it warns at run time).
  server.registerTool(
    "old",
    { inputSchema: attached.inputs(z.object({ table: z.string() })) },
    attached.events("old", async ({ table }, call) => {
      expectTypeOf(call).toEqualTypeOf<ToolCall<ServerContext>>();
      await call.subscribe(news, { table });
      return "ok";
    }),
  );
});
