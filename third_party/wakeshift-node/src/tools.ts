// @wakeshift/sdk/tools: Wakeshift for a runtime with no MCP SDK server
// attach can take: a hand-rolled JSON-RPC runtime, or a framework that
// takes tool definitions and writes tools/list itself (xmcp). A low-level
// Server from either MCP SDK needs none of this: pass it to wake.attach.
//
//   const tools = attachTools(wake, { identify, wrap: { watch_crawl: [crawlFinished] } })
//   // A runtime that answers JSON-RPC itself:
//   await tools.list(myCatalog)                                   // your tools amended, plus the built-ins
//   await tools.call(params, { meta, headers, clientInfo, authInfo }, (p) => myDispatch(p))
//   // A framework that takes tool definitions and owns tools/list (xmcp):
//   tools.tool("watch_crawl", { description, inputSchema, subscribes: [crawlFinished] }, handler) // { schema, metadata, handler }
//   tools.builtins()                                              // the built-in tools' definitions
//
// Every context field is optional: meta and headers feed recognition,
// clientInfo names the client, authInfo reaches identify and must already
// be verified. This path answers no MCP Events and serves no wait tool.
// Nothing here imports an MCP SDK.

import type { AuthInfo, CallToolResult } from "./mcp.js";
import { getObjectShape, type AnySchema, type ZodRawShapeCompat } from "./zod.js";
import { attachHost, checkAttach, withSubscribes, type BuiltinTools, type Identity } from "./events.js";
import { is2026, toolsHost, v1Req, type Handler, type Req } from "./host.js";
import { RESULT_TYPE } from "./spec.js";
import { DisabledCall, WAKE_KEYS, checkSubscribes, withWakeFields, type AnyEvent, type InferArgs, type ToolCall, type ToolLayer, type ToolResult } from "./toolcall.js";
import { CONVERSATION_ARG, HARNESS_ARG, SESSION_ARG, type Wakeshift } from "./wakeshift.js";
import { UsageError } from "./errors.js";

const CLIENT_INFO_META = "io.modelcontextprotocol/clientInfo";

/**
 * A tool call's request context, as your runtime knows it. Every field is
 * optional: meta (the call's _meta, with the 2026-07-28 envelope's
 * io.modelcontextprotocol/* keys in it) and headers feed recognition;
 * clientInfo names the client; authInfo is the verified token, which
 * identify gets. A framework's own context (xmcp's, with _meta and
 * requestInfo.headers) reads the same.
 */
export interface ToolsContext {
  meta?: Record<string, unknown>;
  headers?: Record<string, string | string[] | undefined>;
  clientInfo?: { name?: string; version?: string };
  /** Already verified by your runtime: Wakeshift doesn't check it. */
  authInfo?: AuthInfo;
  signal?: AbortSignal;
  [k: string]: unknown;
}

/** attachTools' options. */
export interface ToolsAttachOptions {
  /** Who is behind a request: { user }, your own ID for the person, from your own sign-in. It may be async. */
  identify?(ctx: ToolsContext): Identity | undefined | Promise<Identity | undefined>;
  /** Your tools that subscribe, by name, each with the events it subscribes to: call answers them with Wakeshift's block. */
  wrap?: Record<string, readonly AnyEvent[]> | ((tool: string) => readonly AnyEvent[] | undefined);
  /** Refuse Wakeshift's part of a call identify gives no user for. Needs identify. */
  requireUser?: boolean;
  /** Which of Wakeshift's own tools builtins and list give (default: all three). There's no wait tool here. */
  builtinTools?: Omit<BuiltinTools, "wait"> | false;
}

/** A tool as tools/list lists it. */
export interface ToolDefinition {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
  annotations?: Record<string, unknown>;
  [k: string]: unknown;
}

/** A tools/call's params. */
export interface ToolCallParams {
  name: string;
  arguments?: Record<string, unknown>;
  _meta?: Record<string, unknown>;
  [k: string]: unknown;
}

/** A tool as a framework that takes definitions wants it (xmcp's tool file: schema, metadata, default handler). */
export interface ToolsTool {
  /** The tool's input, a zod shape with Wakeshift's arguments in it (xmcp: `export const schema = tool.schema`). */
  schema: ZodRawShapeCompat;
  /** Its name, description and annotations (xmcp: `export const metadata = tool.metadata`). */
  metadata: { name: string; description: string; annotations?: Record<string, unknown> };
  /** Its handler (xmcp: `export default tool.handler`). */
  handler: (args: Record<string, unknown>, ctx: ToolsContext) => Promise<CallToolResult>;
  /** Its output's JSON Schema, for a built-in: its results' structuredContent matches it. */
  outputSchema?: Record<string, unknown>;
}

// toolsReq reads a context: ToolsContext's fields, or a framework's
// 1.x-shaped context (_meta, requestInfo.headers), with the client's info
// put where Wakeshift looks for it.
function toolsReq(raw: unknown): Req {
  const c = (raw ?? {}) as ToolsContext & { _meta?: Record<string, unknown>; requestInfo?: { headers?: Record<string, string | string[] | undefined> } };
  const req = v1Req({ _meta: c.meta ?? c._meta, requestInfo: { headers: lowerKeys(c.headers ?? c.requestInfo?.headers) }, authInfo: c.authInfo, signal: c.signal });
  const info = c.clientInfo;
  if (!info?.name || (req.meta && CLIENT_INFO_META in req.meta)) return req;
  return { ...req, meta: { ...(req.meta ?? {}), [CLIENT_INFO_META]: info } };
}

function lowerKeys(h: Record<string, string | string[] | undefined> | undefined): Record<string, string | string[] | undefined> | undefined {
  if (!h) return h;
  return Object.fromEntries(Object.entries(h).map(([k, v]) => [k.toLowerCase(), v]));
}

const CATALOG = Symbol("catalog");
const DISPATCH = Symbol("dispatch");

// The wake arguments tools/list leaves off each built-in.
const BUILTIN_DROPS: Record<string, string[]> = {
  unsubscribe_events: [HARNESS_ARG, CONVERSATION_ARG],
  list_subscriptions: [HARNESS_ARG, CONVERSATION_ARG],
};

/** Wakeshift for a runtime with no MCP server: make it with attachTools. */
export class ToolsAttachment {
  /** @internal */
  constructor(
    private readonly wake: Wakeshift,
    private readonly handlers: Map<string, Handler> | null,
    private readonly layer: ToolLayer<ToolsContext> | null,
    private readonly own: () => Map<string, ToolsTool>,
  ) {}

  /**
   * Your tools as tools/list lists them, amended (Wakeshift's arguments,
   * the subscribes sentence), plus the built-ins. The list differs by
   * caller, so on protocol 2026-07-28 answer it with cacheScope: "private"
   * (and ttlMs: 0), whatever an upstream server's list said.
   */
  async list(tools: ToolDefinition[], ctx: ToolsContext = {}): Promise<ToolDefinition[]> {
    if (!this.handlers) return tools;
    const out = await this.handlers.get("tools/list")!({ method: "tools/list", params: {}, [CATALOG]: tools }, ctx);
    return out.tools as ToolDefinition[];
  }

  /**
   * Answers a tools/call: a built-in itself; any other through dispatch,
   * your own handler, with Wakeshift's arguments stripped. A tool in wrap
   * runs as a call that subscribes (wake.currentCall() finds it), and its
   * result gets Wakeshift's block. On protocol 2026-07-28 (ctx.meta, or the
   * params' _meta, says so) a result without resultType gets
   * resultType: "complete".
   *
   * A JSON-RPC error or an HTTP refusal (a 401) is yours to send: throw
   * from dispatch, and call rethrows that error unchanged, after rolling
   * back anything the call subscribed, for your runtime to answer as it
   * does for its own tools.
   */
  async call(params: ToolCallParams, ctx: ToolsContext, dispatch: (params: ToolCallParams) => Promise<CallToolResult>): Promise<CallToolResult> {
    if (!this.handlers) return dispatch(params);
    const out = await this.handlers.get("tools/call")!({ method: "tools/call", params, [DISPATCH]: dispatch }, ctx ?? {});
    // Protocol 2026-07-28 results carry resultType (the official 2.x
    // clients refuse one without it): the built-ins' answers, and any
    // dispatch gave without one.
    const meta = (ctx ?? {}).meta ?? (ctx as { _meta?: Record<string, unknown> } | undefined)?._meta ?? params?._meta;
    if (out && typeof out === "object" && out.resultType === undefined && is2026(toolsReq({ ...(ctx ?? {}), meta }))) return { ...out, resultType: RESULT_TYPE };
    return out;
  }

  /**
   * A tool that subscribes, as a framework that takes definitions wants it
   * (xmcp): its schema with Wakeshift's arguments, its metadata with the
   * subscribes sentence, and its handler, which gets your arguments, the
   * context and a ToolCall, and answers with Wakeshift's block.
   */
  tool<const E extends readonly AnyEvent[], I extends ZodRawShapeCompat | AnySchema = ZodRawShapeCompat>(
    name: string,
    config: { description?: string; inputSchema?: I; annotations?: Record<string, unknown>; subscribes: E },
    handler: (args: InferArgs<I>, ctx: ToolsContext, call: ToolCall<ToolsContext, E[number]>) => ToolResult | Promise<ToolResult>,
  ): ToolsTool {
    if (!name || typeof name !== "string") throw new UsageError("config", "tools.tool needs the tool's name");
    const events = checkSubscribes(this.wake, name, config?.subscribes);
    const input = (config.inputSchema ?? {}) as ZodRawShapeCompat | AnySchema;
    // As tools/list scopes them: all three for a tool that subscribes, only
    // session_key for one with subscribes: [].
    const withArgs = this.layer ? withWakeFields(input, "inputs", true, events.length > 0 ? WAKE_KEYS : [SESSION_ARG]) : input;
    const schema = (getObjectShape(withArgs) ?? withArgs) as ZodRawShapeCompat;
    const metadata: ToolsTool["metadata"] = { name, description: this.layer ? withSubscribes(config.description ?? "", events.map((e) => e.description)) : (config.description ?? "") };
    if (config.annotations) metadata.annotations = config.annotations;
    const layer = this.layer;
    const h = handler as unknown as (args: unknown, ctx: ToolsContext, call: unknown) => ToolResult | Promise<ToolResult>;
    return {
      schema,
      metadata,
      handler: async (args, ctx) => {
        if (!layer) {
          const r = await h(args, ctx, new DisabledCall(this.wake, name, ctx, events));
          return typeof r === "string" ? { content: [{ type: "text", text: r }] } : r;
        }
        const { args: appArgs, wake } = layer.strip(name, args ?? {});
        const scope = await layer.begin(name, wake, ctx, events);
        const out = (await layer.run(scope, ctx, async () => {
          const r = await h(appArgs, ctx, scope);
          if (typeof r === "string") {
            scope.stringResult = true;
            return { content: [{ type: "text", text: r }] };
          }
          return r;
        })) as CallToolResult;
        // A framework (xmcp) may check structuredContent against the tool's
        // output schema after the handler, refusing fields it doesn't name:
        // the block stays in the content and _meta only.
        const sc = out?.structuredContent;
        if (sc && typeof sc === "object" && !Array.isArray(sc) && "wakeshift" in sc) {
          const { wakeshift: _block, ...rest } = sc as Record<string, unknown>;
          return { ...out, structuredContent: rest };
        }
        return out;
      },
    };
  }

  /** The built-in tools, as a framework that takes definitions wants them. */
  builtins(): ToolsTool[] {
    return [...this.own().values()];
  }
}

/**
 * Attaches Wakeshift to a runtime with no MCP server, after your last
 * defineEvent. Returns list and call for a runtime that answers JSON-RPC,
 * and tool and builtins for a framework that takes tool definitions.
 */
export function attachTools(wake: Wakeshift, opts: ToolsAttachOptions = {}): ToolsAttachment {
  const events = wake.serveEvents();
  checkAttach(wake, events, opts as never, "attachTools", ["identify", "requireUser", "wrap", "builtinTools"]);
  if (!wake.enabled) return new ToolsAttachment(wake, null, null, () => new Map());
  const { host, handlers } = toolsHost(toolsReq);
  // The runtime's own tools/list and tools/call, beneath Wakeshift's layers.
  handlers.set("tools/list", async (request: any) => ({ tools: request?.[CATALOG] ?? [] }));
  handlers.set("tools/call", async (request: any) => request[DISPATCH](request.params));
  const layer = attachHost<ToolsContext>(wake, host, events, { identify: opts.identify, requireUser: opts.requireUser, wrap: opts.wrap, builtinTools: opts.builtinTools });
  const registry = host.registry!;
  const own = () => {
    const out = new Map<string, ToolsTool>();
    for (const [name, t] of registry.tools) {
      if (!t.enabled) continue;
      const metadata: ToolsTool["metadata"] = { name, description: t.description };
      if (t.annotations) metadata.annotations = t.annotations;
      // The schema as tools/list scopes it (unsubscribe_events: only
      // session_key; list_subscriptions: only session_key). A framework that validates
      // with it drops the arguments it doesn't list, which these tools don't need.
      const drop = BUILTIN_DROPS[name] ?? [];
      const shape = Object.fromEntries(Object.entries(getObjectShape(t.schema) ?? {}).filter(([k]) => !drop.includes(k)));
      const tool: ToolsTool = { schema: shape as ZodRawShapeCompat, metadata, handler: (args, ctx) => t.cb(args ?? {}, ctx ?? {}) };
      if (t.outputSchema) tool.outputSchema = t.outputSchema;
      out.set(name, tool);
    }
    return out;
  };
  return new ToolsAttachment(wake, handlers, layer, own);
}

export type { Identity, ToolCall };
