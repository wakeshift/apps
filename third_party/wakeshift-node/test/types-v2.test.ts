// What the compiler infers for tools that subscribe on
// @modelcontextprotocol/server 2.x (types.test.ts does the same for 1.x):
// checked by tsc (npm test compiles this file). Nothing here runs against a
// core.
import { test } from "node:test";
import { expectTypeOf } from "expect-type";
import { McpServer, Server, type CallToolResult, type RegisteredTool, type ServerContext } from "@modelcontextprotocol/server";
import { z } from "zod";
import { Wakeshift, type Attachment, type Subscription, type ToolCall } from "../src/index.js";

const seed = Buffer.alloc(32, 9).toString("base64url");

function setup() {
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const turn = wake.defineEvent("game.turn", {
    description: "Your move",
    args: z.object({ table: z.string() }),
    payload: z.object({ toCall: z.number() }),
    text: (t) => `${t.toCall} to call`,
    personal: true,
  });
  const news = wake.defineEvent("table.news", { description: "News", args: { table: z.string() }, payload: z.object({ text: z.string() }), text: (p) => p.text });
  const server = new McpServer({ name: "t", version: "1" });
  // identify gets 2.x's own context: the token in ctx.http.authInfo.
  const attached = wake.attach(server, {
    identify: (ctx) => {
      expectTypeOf(ctx).toEqualTypeOf<ServerContext>();
      return { user: ctx.http?.authInfo?.clientId };
    },
  });
  return { wake, turn, news, server, attached };
}

test("registerTool's types on the MCP SDK 2.x", () => {
  const { turn, news, attached, server } = setup();
  expectTypeOf(attached).toEqualTypeOf<Attachment<McpServer>>();

  // A zod object, as 2.x takes it (any Standard Schema): the handler's arguments are its output.
  const registered = attached.registerTool(
    "join",
    { description: "Join a table", inputSchema: z.object({ table: z.string(), seats: z.number().optional() }), subscribes: [turn, news] },
    async (args, ctx, call) => {
      expectTypeOf(args).toEqualTypeOf<{ table: string; seats?: number | undefined }>();
      expectTypeOf(ctx).toEqualTypeOf<ServerContext>();
      expectTypeOf(call).toEqualTypeOf<ToolCall<ServerContext, typeof turn | typeof news>>();
      const sub = await call.subscribe(turn, { table: args.table });
      expectTypeOf(sub).toEqualTypeOf<Subscription>();
      // @ts-expect-error: the event's arguments are checked
      await call.subscribe(turn, { tabel: "x" });
      return `Seated at ${args.table}.`;
    },
  );
  expectTypeOf(registered).toEqualTypeOf<RegisteredTool>();

  // A Standard Schema that isn't zod: its declared output type.
  const standard = { "~standard": { version: 1, vendor: "x", validate: (v: unknown) => ({ value: v as { n: number } }), types: { input: {} as { n: number }, output: {} as { n: number } } } } as const;
  attached.registerTool("std", { inputSchema: standard as any as typeof standard, subscribes: [] }, async (args) => {
    expectTypeOf(args).toEqualTypeOf<{ n: number }>();
    return "ok";
  });

  // With an outputSchema: a CallToolResult whose structuredContent matches.
  attached.registerTool("seat", { inputSchema: z.object({ table: z.string() }), outputSchema: z.object({ table: z.string() }), subscribes: [] }, async ({ table }) => {
    return { content: [], structuredContent: { table } } satisfies CallToolResult;
  });

  // An error result needs no structuredContent, as in the MCP SDK's registerTool.
  attached.registerTool("seat4", { outputSchema: z.object({ table: z.string() }), subscribes: [] }, async () => ({ content: [{ type: "text", text: "No such table." }], isError: true }));
  // As the MCP SDK's own registerTool: any CallToolResult, a vendor's own result type included.
  attached.registerTool("seat6", { outputSchema: z.object({ table: z.string() }), subscribes: [] }, async () => ({ content: [] }));
  type VendorResult = { content: { type: "text"; text: string }[]; structuredContent?: Record<string, unknown>; isError?: boolean };
  const vendorHandler = async (): Promise<VendorResult> => ({ content: [{ type: "text", text: "ok" }] });
  server.registerTool("vendor_host", { outputSchema: z.object({ table: z.string() }) }, vendorHandler);
  attached.registerTool("vendor", { outputSchema: z.object({ table: z.string() }), subscribes: [] }, vendorHandler);
  // @ts-expect-error: a tool with an outputSchema doesn't answer with a string
  attached.registerTool("seat7", { outputSchema: z.object({ table: z.string() }), subscribes: [] }, async () => "x");

  // No inputSchema: (ctx, call).
  attached.registerTool("look", { subscribes: [news] }, async (ctx, call) => {
    expectTypeOf(ctx).toEqualTypeOf<ServerContext>();
    await call.subscribe(news, { table: "oak" });
    return "A table.";
  });
});

test("attach on 2.x's low-level Server: identify and currentCall get its ctx", () => {
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const turn = wake.defineEvent("game.turn", { description: "Your move", args: z.object({ table: z.string() }), payload: z.object({ toCall: z.number() }), text: () => "x" });
  const low = new Server({ name: "t", version: "1" }, { capabilities: { tools: {} } });
  const attached = wake.attach(low, {
    identify: (ctx) => {
      expectTypeOf(ctx).toEqualTypeOf<ServerContext>();
      return undefined;
    },
    wrap: { join: [turn] },
  });
  expectTypeOf(attached).toEqualTypeOf<Attachment<Server>>();
  const use = (ctx: ServerContext) => {
    expectTypeOf(wake.currentCall(ctx)).toEqualTypeOf<ToolCall<ServerContext>>();
    expectTypeOf(wake.currentCall(ctx).ctx).toEqualTypeOf<ServerContext>();
  };
  void use;
});
