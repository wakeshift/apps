// @wakeshift/sdk/mastra: Wakeshift in a Mastra MCPServer (@mastra/mcp 2.x).
//
// MCPServer builds a low-level MCP Server (@modelcontextprotocol/server
// 2.x) for each request and fills it in itself; it never hands it to the
// app, and its public extension points (Mastra tools, its own events) can't
// carry what Wakeshift needs: a tool's result is Mastra's conversion of
// what the tool returned, without _meta, which Codex's and Claude Code's
// hooks read Wakeshift's beacon from; and there's no way to add a method
// (tasks/get for the wait tool) or answer a tool's call another way.
//
// So serve() takes the one seam there is: MCPServer's createServerInstance,
// a private method every transport (HTTP, stdio) builds its servers with.
// Each server it builds gets what attach gives an McpServer, on that
// server's request handlers (host.ts's lowServerHost): attach's own tools,
// MCP Events, the wait tool and the 2026-07-28 tools/list changes; and the
// tools wrapped with attached.events get Wakeshift's block in their results.
// serve() checks @mastra/mcp's version and that the seam is there, and the
// compatibility suite (sdk/compat, ts-mastra) runs it against Mastra's
// newest release nightly, so a Mastra release that moves it shows up there.
//
// Mastra's own MCP Events (MCPServer's events option) keep working beside
// Wakeshift's: events/list lists both, and every other events/* request
// goes to whichever serves the event it names (Wakeshift's, if both do).
//
//   const attached = attachMastra(wake, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }) })
//   const watchCi = createTool({
//     id: "watch_ci",
//     description: "Watch CI on a repo",
//     inputSchema: attached.inputs(z.object({ owner: z.string(), repo: z.string() })),
//     execute: attached.events("watch_ci", { subscribes: [ciFinished] }, async ({ owner, repo }, call) => {
//       await call.subscribe(ciFinished, { owner, repo })
//       return `Watching CI on ${owner}/${repo}.`
//     }),
//   })
//   const server = attached.serve(new MCPServer({ id: "ci", name: "CI", version: "1.0.0", tools: { watch_ci: watchCi } }))

import { createRequire } from "node:module";
import type * as z3 from "zod/v3";
import type * as z4 from "zod/v4";
import type { ToolCallExtra } from "./mcp.js";
import type { ZodRawShapeCompat } from "./zod.js";
import { attachHost, checkAttach, type AttachOptions, type AttachTools, type BuiltinTool, type Identity } from "./events.js";
import { lowServerHost, toolsOnlyHost, v2Req, type Handler } from "./host.js";
import { Attached, CallEnded, type AnyWakeEvent, type CallScope, type ToolCall, type ToolEventsOptions, type WakeArgs, type WakeInputs, type WakeInputsV3, type WakeInputsV4, type WakeKey, type WakeOutputShape, type WakeOutputV3, type WakeOutputV4 } from "./toolcall.js";
import type { Wakeshift } from "./wakeshift.js";

/** The @mastra/mcp releases serve() is built for: 2.x, from 2.7. */
export const MASTRA_MCP_RANGE = ">=2.7.0 <3.0.0";

/**
 * The request context Wakeshift reads in Mastra: the MCP SDK 2.x's
 * ServerContext (`ctx`) of the request, the verified token in
 * `ctx.http?.authInfo`. A Mastra tool sees the same as `context.mcp.extra`.
 */
export type MastraContext = ToolCallExtra;

/** attachMastra's options: attach's, with identify and events.native getting the request's ctx. */
export type MastraAttachOptions = AttachOptions<MastraContext>;

/** The second argument Mastra passes a tool's execute: what Wakeshift reads of it. */
export interface MastraToolContext {
  mcp?: { extra?: { mcpReq?: object; [k: string]: any }; [k: string]: any };
  [k: string]: any;
}

const EVENTS_METHODS = ["events/list", "events/subscribe", "events/unsubscribe", "events/poll", "events/stream"] as const;

/** @internal Whether a version of @mastra/mcp is one serve() is built for. */
export function supportedMastra(version: string): boolean {
  const m = /^(\d+)\.(\d+)\./.exec(version);
  return !!m && Number(m[1]) === 2 && Number(m[2]) >= 7;
}

// installedMastra is the @mastra/mcp version this process loads, if it can
// tell (a bundled app may not ship package.json files).
function installedMastra(): string | undefined {
  try {
    return (createRequire(import.meta.url)("@mastra/mcp/package.json") as { version?: string }).version;
  } catch {
    return undefined;
  }
}

type Scope = { call: CallScope<MastraContext> };

/** Wakeshift in a Mastra MCPServer: make it with attachMastra, then serve your MCPServer with it. */
export class MastraAttached {
  private readonly wrapped = new Set<string>();
  // The events each wrapped tool declares (subscribes), by tool.
  private readonly declared = new Map<string, readonly AnyWakeEvent[]>();
  // The call each wrapped tool's execute belongs to, by its request (ctx.mcpReq,
  // which Mastra passes the tool as context.mcp.extra.mcpReq).
  private readonly scopes = new WeakMap<object, Scope>();
  // Only for inputs and output, which need no server.
  private readonly shapes: Attached<MastraContext>;
  private readonly served = new WeakSet<object>();

  /** @internal Use attachMastra. */
  constructor(
    private readonly wake: Wakeshift,
    private readonly opts: MastraAttachOptions,
  ) {
    const host = toolsOnlyHost({ framework: "mastra", req: v2Req, registerTool: () => {} });
    this.shapes = new Attached<MastraContext>({ wake, host, identify: () => undefined, clientOf: () => "", events: {}, warnOnce: () => {} });
  }

  /**
   * A tool's input schema plus Wakeshift's three optional arguments
   * (session_key, harness and conversation_id), for a wrapped tool's
   * inputSchema: `inputSchema: attached.inputs(z.object({ … }))`.
   */
  inputs<O extends z4.ZodObject>(schema: O): z4.ZodObject<O["shape"] & WakeInputsV4, O["_zod"]["config"]>;
  inputs<O extends z3.AnyZodObject>(schema: O): z3.ZodObject<O["shape"] & WakeInputsV3, O["_def"]["unknownKeys"], O["_def"]["catchall"]>;
  inputs<S extends ZodRawShapeCompat>(shape: S): S & WakeInputs;
  inputs(schema: any): any {
    return this.shapes.inputs(schema);
  }

  /** A tool's output schema plus an optional wakeshift string, for a wrapped tool's outputSchema. Optional: see attach's. */
  output<O extends z4.ZodObject>(schema: O): z4.ZodObject<O["shape"] & WakeOutputV4, O["_zod"]["config"]>;
  output<O extends z3.AnyZodObject>(schema: O): z3.ZodObject<O["shape"] & WakeOutputV3, O["_def"]["unknownKeys"], O["_def"]["catchall"]>;
  output<S extends ZodRawShapeCompat>(shape: S): S & WakeOutputShape;
  output(schema: any): any {
    return this.shapes.output(schema);
  }

  /**
   * Wraps a Mastra tool's execute, for the tool your MCPServer serves under
   * the same name (its key in `tools`): `execute: attached.events("watch_ci",
   * async (args, call) => …)`. handler gets your own arguments, a ToolCall
   * and Mastra's tool context, and returns what a Mastra tool returns (a
   * string, or your output). The reply an MCP client gets carries what
   * Wakeshift adds for the conversation; a call that fails ends the
   * subscriptions it made. Its inputSchema must come from attached.inputs.
   * Run outside an MCP request (by an agent, a workflow or Studio), the
   * handler gets a call whose subscribe throws.
   *
   * subscribes lists the events the tool subscribes to, as attach's
   * attached.events takes it: call.subscribe takes only those, and
   * tools/list says the tool is how the app notifies the agent of them.
   * `execute: attached.events("watch_ci", { subscribes: [ciFinished] }, async (args, call) => …)`.
   */
  events<P extends Record<string, any> = Record<string, any>, R = unknown, const E extends readonly AnyWakeEvent[] = readonly AnyWakeEvent[]>(
    name: string,
    opts: ToolEventsOptions<E>,
    handler: (args: P, call: ToolCall<MastraContext, E[number]>, context: MastraToolContext) => R | Promise<R>,
  ): (input: P & WakeArgs, context?: any) => Promise<R>;
  events<P extends Record<string, any> = Record<string, any>, R = unknown>(
    name: string,
    handler: (args: P, call: ToolCall<MastraContext>, context: MastraToolContext) => R | Promise<R>,
  ): (input: P & WakeArgs, context?: any) => Promise<R>;
  events(name: string, optsOrHandler: unknown, maybeHandler?: unknown): (input: any, context?: any) => Promise<any> {
    if (!name || typeof name !== "string") throw new Error("wakeshift: attached.events needs the tool's name, as your MCPServer serves it");
    const declared = typeof optsOrHandler === "function" ? null : this.shapes.checkDeclared(name, optsOrHandler);
    const handler = (typeof optsOrHandler === "function" ? optsOrHandler : maybeHandler) as (args: any, call: ToolCall<MastraContext>, context: MastraToolContext) => unknown;
    if (typeof handler !== "function") throw new Error("wakeshift: attached.events needs a handler");
    this.wrapped.add(name);
    if (declared) this.declared.set(name, declared);
    else this.declared.delete(name);
    return async (input, context) => {
      const key = context?.mcp?.extra?.mcpReq;
      const scope = key && typeof key === "object" ? this.scopes.get(key) : undefined;
      const args: Record<string, unknown> = {};
      for (const [k, v] of Object.entries((input ?? {}) as Record<string, unknown>)) {
        if (k !== "session_key" && k !== "harness" && k !== "conversation_id") args[k] = v;
      }
      const call = scope && scope.call.tool === name ? scope.call : detachedCall(name, context);
      return handler(args, call, context ?? {});
    };
  }

  /**
   * Serves Wakeshift from your MCPServer (@mastra/mcp 2.7 or later, 2.x),
   * after your last defineEvent: attach's own tools, MCP Events for clients
   * that speak it, the wait tool, and your wrapped tools' blocks. Returns
   * the server. Call it once, before the server takes requests.
   */
  serve<S extends object>(server: S): S {
    const version = installedMastra();
    if (version && !supportedMastra(version)) {
      throw new Error(`wakeshift: attachMastra serves @mastra/mcp ${MASTRA_MCP_RANGE}, and this is ${version}: check for an @wakeshift/sdk that supports it`);
    }
    const s = server as { createServerInstance?: unknown };
    if (typeof s.createServerInstance !== "function") {
      throw new Error(
        `wakeshift: attached.serve takes Mastra's MCPServer (@mastra/mcp ${MASTRA_MCP_RANGE}), and this one has no createServerInstance${version ? ` (@mastra/mcp ${version})` : ""}: check for a newer @wakeshift/sdk`,
      );
    }
    if (this.served.has(server)) return server;
    this.served.add(server);
    const build = s.createServerInstance as (...args: unknown[]) => unknown;
    const self = this;
    s.createServerInstance = function (this: unknown, ...args: unknown[]) {
      const low = build.apply(this, args);
      self.attachTo(low);
      return low;
    };
    return server;
  }

  // attachTo gives one server MCPServer built what attach gives an McpServer.
  // A failure leaves the request served without Wakeshift, and says why once.
  private attachTo(low: any): void {
    try {
      const host = lowServerHost(low, "Mastra's MCPServer");
      const handlers = host.handlers()!;
      const theirs = new Map<string, Handler>();
      for (const m of EVENTS_METHODS) {
        const h = handlers.get(m);
        if (h) theirs.set(m, h);
      }
      const events = this.wake.serveEvents();
      const attached = attachHost<MastraContext>(this.wake, host, events, this.opts);
      for (const name of this.wrapped) attached.markWrapped(name, this.declared.get(name) ?? null);
      if (theirs.size > 0) shareEvents(handlers, theirs, new Set(events.map((e) => e.name)));
      this.wrapCalls(handlers, attached);
    } catch (err) {
      this.wake.warnOnce("mastra-attach", `wakeshift: couldn't attach to the server Mastra's MCPServer built, so its requests are served without Wakeshift: ${String((err as Error)?.message ?? err)}`);
    }
  }

  // wrapCalls runs each wrapped tool's call in a scope its execute finds, and
  // adds Wakeshift's block to the result Mastra makes of what it returned.
  private wrapCalls(handlers: Map<string, Handler>, attached: Attached<MastraContext>): void {
    const inner = handlers.get("tools/call");
    if (!inner) return;
    Map.prototype.set.call(handlers, "tools/call", async (request: any, ctx: any) => {
      const name = request?.params?.name;
      if (typeof name !== "string" || !this.wrapped.has(name)) return inner(request, ctx);
      const { call } = attached.begin(name, (request?.params?.arguments ?? {}) as Record<string, unknown>, ctx);
      const key = ctx?.mcpReq && typeof ctx.mcpReq === "object" ? (ctx.mcpReq as object) : undefined;
      if (key) this.scopes.set(key, { call });
      let result: any;
      try {
        result = await inner(request, ctx);
      } catch (err) {
        await attached.fail(call);
        throw err;
      } finally {
        if (key) this.scopes.delete(key);
      }
      if (!result || typeof result !== "object" || !Array.isArray(result.content)) {
        // Not a tool's answer (a task handle, an input request): no block to
        // add, so nothing the call subscribed stays.
        await attached.fail(call);
        this.wake.warnOnce(
          "mastra-result:" + name,
          `wakeshift: ${name} answered with something other than a tool result (a Mastra task, or a request for input), so what it subscribed was rolled back: a wrapped tool must answer in one call`,
        );
        return result;
      }
      return attached.finish(call, result, undefined);
    });
  }
}

// shareEvents serves Mastra's own MCP Events beside Wakeshift's: events/list
// lists both (Wakeshift's first, on the first page), and every other method
// goes to whichever serves the event it names.
function shareEvents(handlers: Map<string, Handler>, theirs: Map<string, Handler>, ours: Set<string>): void {
  for (const [method, their] of theirs) {
    const mine = handlers.get(method);
    if (!mine || mine === their) continue;
    if (method === "events/list") {
      Map.prototype.set.call(handlers, method, async (request: any, ctx: any) => {
        if (request?.params?.cursor !== undefined) return their(request, ctx);
        const [a, b] = await Promise.all([mine(request, ctx), their(request, ctx)]);
        const listed = [...(a?.events ?? []), ...(b?.events ?? []).filter((e: { name?: string }) => !ours.has(e?.name ?? ""))];
        return { ...b, ...a, events: listed, ...(b?.nextCursor !== undefined ? { nextCursor: b.nextCursor } : {}) };
      });
      continue;
    }
    Map.prototype.set.call(handlers, method, (request: any, ctx: any) => (ours.has(request?.params?.name) ? mine(request, ctx) : their(request, ctx)));
  }
}

// detachedCall is the call a wrapped tool's handler gets outside an MCP
// tools/call (an agent, a workflow or Studio ran the tool): nothing to subscribe.
function detachedCall(tool: string, context: MastraToolContext | undefined): ToolCall<MastraContext> {
  const refuse = () =>
    Promise.reject(
      new Error(
        `wakeshift: ${tool} ran outside an MCP client's call to a server attachMastra serves (from an agent, a workflow or Studio?), so there's no conversation to subscribe`,
      ),
    );
  return {
    user: undefined,
    tool,
    native: false,
    nativeConversation: null,
    session: null,
    recognized: null,
    extra: (context?.mcp?.extra ?? {}) as MastraContext,
    subscribe: refuse,
    unsubscribe: refuse,
    resolveSession: refuse,
  };
}

/**
 * Wakeshift for a Mastra MCPServer (@mastra/mcp 2.7 or later). Returns the
 * handle your tools use (attached.inputs, attached.events) and that serves
 * your MCPServer (attached.serve). Options are attach's; identify and
 * events.native get the request's MCP context (ctx: the verified token is
 * ctx.http?.authInfo).
 */
export function attachMastra(wake: Wakeshift, opts: MastraAttachOptions = {}): MastraAttached {
  checkAttach(wake.serveEvents(), opts, "attachMastra");
  return new MastraAttached(wake, opts);
}

export { CallEnded, type AttachTools, type BuiltinTool, type Identity, type ToolCall, type WakeArgs };
