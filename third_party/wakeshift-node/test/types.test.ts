// What the compiler infers for wrapped tools (attached.events): checked by
// tsc (npm test compiles this file), against the MCP SDK installed: CI
// checks the lowest version the peer range allows too. Nothing here runs
// against a core.
import { test } from "node:test";
import { expectTypeOf } from "expect-type";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import * as z3 from "zod/v3";
import { Wakeshift, type Emitted, type Recipient, type Subscription, type ToolCall, type ToolCallExtra } from "../src/index.js";

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
  const attached = wake.attach(server, { identify: () => ({ user: "ada" }) });
  return { wake, turn, server, attached };
}

test("wrapped tools' types", () => {
  const { turn, server, attached } = setup();

  // Without outputSchema: the handler's arguments are the tool's own,
  // without Wakeshift's three; call is a ToolCall; a string is a result.
  server.registerTool(
    "join",
    { description: "Join a table", inputSchema: attached.inputs({ table: z.string(), seats: z.number().optional() }) },
    attached.events("join", async (args, call) => {
      // As MCP's own callbacks get them (ShapeOutput): an optional field is present, maybe undefined.
      expectTypeOf(args).toEqualTypeOf<{ table: string; seats: number | undefined }>();
      expectTypeOf(call).toEqualTypeOf<ToolCall>();
      expectTypeOf(call.user).toEqualTypeOf<string | undefined>();
      expectTypeOf(call.extra).toEqualTypeOf<ToolCallExtra>();
      const sub = await call.subscribe(turn, { table: args.table }, { reason: "Playing", ttl: "1h" });
      expectTypeOf(sub).toEqualTypeOf<Subscription>();
      expectTypeOf(await call.unsubscribe(turn, { table: "x" })).toEqualTypeOf<boolean>();
      expectTypeOf(await call.resolveSession()).toEqualTypeOf<string>();
      // @ts-expect-error: the event's arguments are checked
      await call.subscribe(turn, { tabel: "x" });
      // @ts-expect-error: Wakeshift's arguments aren't the handler's
      void args.session_key;
      return `Seated at ${args.table}.`;
    }),
  );

  // With outputSchema (and the typed-output opt-in): a CallToolResult.
  server.registerTool(
    "look",
    { inputSchema: attached.inputs({ table: z.string() }), outputSchema: attached.output({ seats: z.number() }) },
    attached.events("look", ({ table }) => {
      expectTypeOf(table).toEqualTypeOf<string>();
      const out: CallToolResult = { content: [{ type: "text", text: "3" }], structuredContent: { seats: 3 } };
      return out;
    }),
  );

  // An output schema without the opt-in compiles too.
  server.registerTool(
    "count",
    { inputSchema: attached.inputs({}), outputSchema: { n: z.number() } },
    attached.events("count", () => ({ content: [], structuredContent: { n: 1 } })),
  );

  // zod 3 shapes work the same.
  server.registerTool(
    "old",
    { inputSchema: attached.inputs({ table: z3.string() }) },
    attached.events("old", (args) => {
      expectTypeOf(args).toEqualTypeOf<{ table: string }>();
      return "ok";
    }),
  );

  // A tool whose inputSchema lacks Wakeshift's arguments doesn't compile.
  server.registerTool(
    "bare",
    // @ts-expect-error: the input schema must come from attached.inputs (attached.events needs its three keys)
    { inputSchema: { table: z.string() } },
    attached.events("bare", () => "x"),
  );

  // Nor does a handler returning something that isn't a result.
  server.registerTool(
    "wrong",
    { inputSchema: attached.inputs({}) },
    // @ts-expect-error: a string or a CallToolResult
    attached.events("wrong", () => 42),
  );
});

test("emit's recipients' types", () => {
  const { turn } = setup();
  const sends = () => {
    void turn.emit({ table: "oak" }, { toCall: 1 }, { to: "ada" });
    void turn.emit({ table: "oak" }, { toCall: 1 }, { to: { user: "ada", subscription: "s1" } });
    void turn.emit({ table: "oak" }, { toCall: 1 }, { to: ["ada", { user: "bo", subscription: "s2" }, { native: "cy" }] });
    // @ts-expect-error: a subscription needs its user
    void turn.emit({ table: "oak" }, { toCall: 1 }, { to: { subscription: "s1" } });
  };
  void sends;
  expectTypeOf(turn.emit).returns.resolves.toEqualTypeOf<Emitted>();
  expectTypeOf<Emitted["unmatched"]>().toEqualTypeOf<(string | Recipient)[]>();
});

test("0.4.0's removed calls don't compile", () => {
  const { wake, turn } = setup();
  const removed = () => {
    // @ts-expect-error: wake.args is attached.inputs(shape)
    void wake.args;
    // @ts-expect-error: wake.call is gone: a wrapped tool gets its call
    void wake.call;
    // @ts-expect-error: topics are gone: emit an event
    void wake.publish;
    // @ts-expect-error: topics are gone: call.subscribe in a wrapped tool
    void wake.subscribe;
    // @ts-expect-error: topics are gone: event.status
    void wake.topicStatus;
    // @ts-expect-error: the wrapper adds Wakeshift's block itself
    void wake.annotate;
    // @ts-expect-error: event.subscribe is call.subscribe in a wrapped tool
    void turn.subscribe;
    // @ts-expect-error: event.unsubscribe is call.unsubscribe in a wrapped tool
    void turn.unsubscribe;
  };
  void removed;
});
