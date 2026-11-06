// What the compiler infers for tools that subscribe (attached.registerTool)
// on @modelcontextprotocol/sdk 1.x: checked by tsc (npm test compiles this
// file). types-v2.test.ts does the same for 2.x. Nothing here runs against
// a core.
import { test } from "node:test";
import { expectTypeOf } from "expect-type";
import { McpServer, type RegisteredTool } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import type { CallToolResult, ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import { z } from "zod";
import * as z3 from "zod/v3";
import { BUILTIN_TOOL_NAMES, Wakeshift, type AnyEvent, type SharedEvent, type Attachment, type Emitted, type EventArgs, type EventPayload, type LogFields, type Logger, type Recipient, type Subscribed, type Subscription, type ToolCall } from "../src/index.js";
import type { ToolCallExtra } from "../src/mcp.js";

type V1Extra = RequestHandlerExtra<ServerRequest, ServerNotification>;

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
  const attached = wake.attach(server, { identify: () => ({ user: "ada" }) });
  return { wake, turn, news, server, attached };
}

test("registerTool's types: the host's handler with call appended", () => {
  const { turn, news, attached, server } = setup();
  expectTypeOf(attached).toEqualTypeOf<Attachment<McpServer>>();

  // A zod shape, as 1.x takes it: the handler's arguments are the tool's
  // own (ShapeOutput: an optional field is present, maybe undefined), ctx is
  // 1.x's extra, call a ToolCall over the declared events.
  const registered = attached.registerTool(
    "join",
    { description: "Join a table", inputSchema: { table: z.string(), seats: z.number().optional() }, subscribes: [turn] },
    async (args, ctx, call) => {
      expectTypeOf(args).toEqualTypeOf<{ table: string; seats: number | undefined }>();
      expectTypeOf(ctx).toEqualTypeOf<V1Extra>();
      expectTypeOf(call.ctx).toEqualTypeOf<V1Extra>();
      expectTypeOf(call.user).toEqualTypeOf<string | undefined>();
      const sub = await call.subscribe(turn, { table: args.table }, { reason: "Playing", ttl: "1h" });
      expectTypeOf(sub).toEqualTypeOf<Subscription>();
      expectTypeOf(await call.unsubscribe(turn, { table: "x" })).toEqualTypeOf<boolean>();
      expectTypeOf(await call.resolveSession()).toEqualTypeOf<string | null>();
      // @ts-expect-error: the event's arguments are checked
      await call.subscribe(turn, { tabel: "x" });
      // @ts-expect-error: only the declared events (subscribes)
      await call.subscribe(news, { table: "x" });
      // @ts-expect-error: Wakeshift's arguments aren't the handler's
      void args.session_key;
      return `Seated at ${args.table}.`;
    },
  );
  // The host's own RegisteredTool: update, disable and remove work.
  expectTypeOf(registered).toEqualTypeOf<RegisteredTool>();

  // A zod object (zod 4 and zod 3): its output type, through Standard Schema.
  attached.registerTool("obj", { inputSchema: z.object({ n: z.number() }), subscribes: [news] }, async ({ n }) => {
    expectTypeOf(n).toEqualTypeOf<number>();
    return "ok";
  });
  attached.registerTool("obj3", { inputSchema: z3.object({ n: z3.number() }), subscribes: [news] }, async ({ n }) => {
    expectTypeOf(n).toEqualTypeOf<number>();
    return "ok";
  });

  // No inputSchema: (ctx, call).
  attached.registerTool("look", { description: "Look", subscribes: [news] }, async (ctx, call) => {
    expectTypeOf(ctx).toEqualTypeOf<V1Extra>();
    await call.subscribe(news, { table: "oak" });
    return "A table.";
  });

  // With outputSchema: only a CallToolResult whose structuredContent matches.
  attached.registerTool("seat", { inputSchema: { table: z.string() }, outputSchema: { table: z.string() }, subscribes: [] }, async ({ table }) => {
    return { content: [{ type: "text", text: table }], structuredContent: { table } } satisfies CallToolResult;
  });
  // @ts-expect-error: a tool with an outputSchema doesn't answer with a string
  attached.registerTool("seat2", { outputSchema: { table: z.string() }, subscribes: [] }, async () => "x");
  // As the MCP SDK's own registerTool: any CallToolResult (the MCP SDK checks structuredContent at run time).
  attached.registerTool("seat3", { outputSchema: { table: z.string() }, subscribes: [] }, async () => ({ content: [], structuredContent: { seat: 3 } }));
  // A vendor's own result type, which 1.x's registerTool takes (Upload-Post's): optional structuredContent and isError.
  type VendorResult = { content: { type: "text"; text: string }[]; structuredContent?: Record<string, unknown>; isError?: boolean };
  const vendorHandler = async (): Promise<VendorResult> => ({ content: [{ type: "text", text: "ok" }] });
  server.registerTool("vendor_host", { outputSchema: { table: z.string() } }, vendorHandler);
  attached.registerTool("vendor", { outputSchema: { table: z.string() }, subscribes: [] }, vendorHandler);

  // An error result needs no structuredContent, as in the MCP SDK's registerTool.
  attached.registerTool("seat4", { outputSchema: { table: z.string() }, subscribes: [] }, async () => ({ content: [{ type: "text", text: "No such table." }], isError: true }));
  attached.registerTool("seat5", { outputSchema: { table: z.string() }, subscribes: [] }, async ({}, call) => {
    void call;
    if (Math.random() > 2) return { content: [{ type: "text" as const, text: "No such table." }], isError: true as const };
    return { content: [], structuredContent: { table: "t" } };
  });
  attached.registerTool("seat6", { outputSchema: { table: z.string() }, subscribes: [] }, async () => ({ content: [] }));

  // An inputSchema typed any (a schema built at run time) takes args, as one with a schema does.
  const runtimeSchema: any = { channel: z.string() };
  attached.registerTool("dynamic", { inputSchema: runtimeSchema, subscribes: [news] }, async (args, extra, call) => {
    expectTypeOf(extra).toEqualTypeOf<V1Extra>();
    await call.subscribe(news, { table: String(args.channel) });
    return "ok";
  });

  // subscribes: [] is a tool that subscribes to nothing: no call.subscribe.
  attached.registerTool("nothing", { subscribes: [] }, async (_ctx, call) => {
    // @ts-expect-error: nothing declared
    void call.subscribe;
    return "ok";
  });

  // Other host fields pass through unchanged.
  attached.registerTool("annotated", { title: "Annotated", annotations: { readOnlyHint: true }, _meta: { x: 1 }, icons: [], subscribes: [] }, async () => "ok");

  // A handler returning something that isn't a result doesn't compile.
  // @ts-expect-error: a string or a CallToolResult
  attached.registerTool("wrong", { subscribes: [] }, () => 42);
  // subscribes left out: a plain tool, as subscribes: [] (no call.subscribe).
  attached.registerTool("no_subscribes", { description: "x" }, async (_ctx, call) => {
    // @ts-expect-error: nothing declared
    void call.subscribe;
    void call.unsubscribe;
    return "ok";
  });
});

test("currentCall's types: the context given, any of the app's events", () => {
  const { wake, turn } = setup();
  const use = (ctx: V1Extra) => {
    const call = wake.currentCall(ctx);
    expectTypeOf(call).toEqualTypeOf<ToolCall<V1Extra>>();
    void call.subscribe(turn, { table: "oak" });
  };
  void use;
  const bare = () => wake.currentCall();
  expectTypeOf(bare).returns.toEqualTypeOf<ToolCall<ToolCallExtra>>();
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

test("personal events' types: to is required on a personal event, refused on a shared one, optional when personal isn't a literal", () => {
  const { wake, turn, news } = setup();
  const uses = () => {
    void turn.emit({ table: "oak" }, { toCall: 1 }, { to: "ada" });
    // @ts-expect-error: a personal event needs to
    void turn.emit({ table: "oak" }, { toCall: 1 });
    // @ts-expect-error: a personal event needs to, even with other options
    void turn.emit({ table: "oak" }, { toCall: 1 }, { id: "x" });
    void news.emit({ table: "oak" }, { text: "x" });
    void news.emit({ table: "oak" }, { text: "x" }, { id: "x", signal: AbortSignal.timeout(1) });
    // @ts-expect-error: a shared event takes no to
    void news.emit({ table: "oak" }, { text: "x" }, { to: "ada" });
    void turn.createPublishUrl({ table: "oak" }, { to: "ada" });
    // @ts-expect-error: a personal event's publish URL needs to
    void turn.createPublishUrl({ table: "oak" });
    // @ts-expect-error: a shared event's publish URL takes no to
    void news.createPublishUrl({ table: "oak" }, { to: "ada" });
    void turn.status({ table: "oak" }, { to: "ada" });
    // @ts-expect-error: a personal event's status needs to
    void turn.status({ table: "oak" });
    const maybe: boolean = Math.random() > 1;
    const either = wake.defineEvent("x.y", { description: "X", args: z.object({ a: z.string() }), payload: z.object({}), personal: maybe });
    void either.emit({ a: "1" }, {});
    void either.emit({ a: "1" }, {}, { to: "ada" });
  };
  void uses;
});

test("removed calls don't compile", () => {
  const { wake, turn, attached } = setup();
  const removed = () => {
    // @ts-expect-error: wake.args is gone
    void wake.args;
    // @ts-expect-error: topics are gone: emit an event
    void wake.publish;
    // @ts-expect-error: event.subscribe is call.subscribe in a tool that subscribes
    void turn.subscribe;
    // @ts-expect-error: attached.events is attached.registerTool
    void attached.events;
    // @ts-expect-error: attached.inputs is gone: Wakeshift adds its arguments in tools/list
    void attached.inputs;
    // @ts-expect-error: attached.call is wake.currentCall
    void attached.call;
  };
  void removed;
});

test("attach on 1.x's low-level Server: identify gets the fields Wakeshift reads of extra", () => {
  const { wake, turn } = setup();
  const low = new Server({ name: "t", version: "1" }, { capabilities: { tools: {} } });
  const attached = wake.attach(low, {
    identify: (extra) => {
      expectTypeOf(extra).toEqualTypeOf<ToolCallExtra>();
      return { user: extra.authInfo?.clientId };
    },
    wrap: { join: [turn] },
  });
  expectTypeOf(attached).toEqualTypeOf<Attachment<Server>>();
});

test("a logger taking Record<string, unknown> fields, a pino-style adapter, a winston-style logger and console all assign as a Logger without a cast", () => {
  const records: Logger = {
    error: (m: string, f: Record<string, unknown>) => void [m, f],
    warn: (m: string, f: Record<string, unknown>) => void [m, f],
    info: (m: string, f?: Record<string, unknown>) => void [m, f],
  };
  const pino = { warn: (o: Record<string, unknown>, m?: string) => void [o, m], error: (o: object, m?: string) => void [o, m] };
  const pinoAdapter: Logger = { warn: (m, f) => pino.warn(f, m), error: (m, f) => pino.error(f, m) };
  interface WinstonLike {
    error(message: string, ...meta: unknown[]): WinstonLike;
    warn(message: string, ...meta: unknown[]): WinstonLike;
    info(message: string, ...meta: unknown[]): WinstonLike;
    debug(message: string, ...meta: unknown[]): WinstonLike;
  }
  const winston = {} as WinstonLike;
  const fromWinston: Logger = winston;
  const fromConsole: Logger = console;
  expectTypeOf(records).toExtend<Logger>();
  expectTypeOf([pinoAdapter, fromWinston, fromConsole]).toExtend<Logger[]>();
  expectTypeOf<LogFields>().toExtend<Record<string, unknown>>();
});

test("AnyEvent, EventArgs and EventPayload type a helper that takes any event, without any", () => {
  const { turn, news } = setup();
  // A helper over any of the app's events, and one typed by the event it's given.
  const described = (events: readonly AnyEvent[]): string[] => events.map((e) => e.description);
  function argsFor<E extends AnyEvent>(_event: E, args: EventArgs<E>): EventArgs<E> {
    return args;
  }
  expectTypeOf(described([turn, news])).toEqualTypeOf<string[]>();
  expectTypeOf(argsFor(turn, { table: "oak" })).toEqualTypeOf<{ table: string }>();
  expectTypeOf<EventPayload<typeof turn>>().toEqualTypeOf<{ toCall: number }>();
  // @ts-expect-error: turn's arguments have table
  argsFor(turn, { nope: 1 });
});

test("SharedEvent: a helper that emits any shared event, without to; emit on AnyEvent takes to as optional", async () => {
  const { turn, news } = setup();
  const emitAll = async (events: readonly SharedEvent[]) => {
    for (const e of events) await e.emit({ table: "oak" }, { text: "hi" });
  };
  void emitAll;
  void (() => emitAll([news]));
  // @ts-expect-error: a personal event isn't a SharedEvent
  void (() => emitAll([turn]));
  const any: AnyEvent = news;
  void (() => any.emit({ table: "oak" }, { text: "hi" }));
  void (() => any.emit({ table: "oak" }, { text: "hi" }, { to: "u1" }));
  expectTypeOf(BUILTIN_TOOL_NAMES).toEqualTypeOf<readonly ["subscribe_events", "unsubscribe_events", "list_subscriptions", "wakeshift_wait"]>();
});

test("Subscribed's Ctx: onSubscribe annotated with the context's type, no cast", () => {
  const wake = new Wakeshift({ key: `wst_test_chat_${seed}`, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  type ApiKey = { key: string };
  const done = wake.defineEvent("crawl.done", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ ok: z.boolean() }),
    text: () => "done",
    onSubscribe: (s: Subscribed<{ job: string }, ApiKey>) => {
      expectTypeOf(s.context).toEqualTypeOf<ApiKey | undefined>();
      expectTypeOf(s.args.job).toEqualTypeOf<string>();
    },
  });
  // Unannotated, the parameter is still inferred: args from the event, context unknown.
  wake.defineEvent("crawl.started", {
    description: "A crawl started",
    args: z.object({ job: z.string() }),
    payload: z.object({ ok: z.boolean() }),
    text: () => "started",
    onSubscribe: (s) => {
      expectTypeOf(s.args.job).toEqualTypeOf<string>();
      expectTypeOf(s.context).toEqualTypeOf<unknown>();
    },
  });
  wake.defineEvent("crawl.failed", {
    description: "A crawl failed",
    args: z.object({ job: z.string() }),
    payload: z.object({ ok: z.boolean() }),
    text: () => "failed",
    // @ts-expect-error: args the event doesn't have
    onSubscribe: (s: Subscribed<{ job: number }, ApiKey>) => void s,
  });
  expectTypeOf<Subscribed>().toEqualTypeOf<Subscribed<Record<string, unknown>, unknown>>();
  void done;
});
