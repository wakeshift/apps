// @wakeshift/sdk/xmcp: Wakeshift in an xmcp app (https://xmcp.dev).
//
// xmcp builds its MCP server itself, from the files in src/tools, and never
// hands it to the app, so there's nothing for attach to take. What it does
// hand the app is each tool: a file's schema, metadata and default-exported
// handler. So the adapter works through tools alone, xmcp's supported way
// in:
//
//   - a tool that subscribes is a tool file whose schema comes from
//     attached.inputs and whose handler is attached.events(name, handler),
//     which returns the result with Wakeshift's block in it (text and
//     _meta), as attach's wrapper does;
//   - attach's own tools (subscribe_events, unsubscribe_events,
//     list_subscriptions) are tool files too, one line each from
//     attached.tool(name);
//   - who's calling is identify(extra), xmcp's handler context.
//
// xmcp's middleware sees only the methods it lists (tools/*, prompts/*,
// resources/*), and only tools it has, so a server can't answer MCP Events
// (events/*) or the wait tool's tasks/get here: clients that subscribe
// natively (ChatGPT) aren't served, and Claude Code gets its usual wake
// path instead of wakeshift_wait. Nothing here imports xmcp.
//
//   // src/wake.ts
//   export const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY! })
//   export const ciFinished = wake.defineEvent("ci.finished", { … })
//   export const attached = attachXmcp(wake, { identify: (extra) => ({ user: userOf(extra.authInfo) }) })
//
//   // src/tools/watch_ci.ts
//   export const schema = attached.inputs({ owner: z.string(), repo: z.string() })
//   export const metadata = { name: "watch_ci", description: attached.describe("Watch CI on a repo.", { subscribes: [ciFinished] }) }
//   export default attached.events("watch_ci", { subscribes: [ciFinished] }, async ({ owner, repo }, call) => { … })
//
//   // src/tools/subscribe_events.ts (and the same for the other two)
//   const tool = attached.tool("subscribe_events")
//   export const schema = tool.schema
//   export const metadata = tool.metadata
//   export default tool.handler

import type { AuthInfo, CallToolResult } from "./mcp.js";
import type { ZodRawShapeCompat } from "./zod.js";
import { BUILTIN_TOOLS, attachHost, checkAttach, withSubscribes, type AttachTools, type BuiltinTool, type Identity } from "./events.js";
import { toolsOnlyHost, v1Req, type Req } from "./host.js";
import { WAKESHIFT_FIELD, type AnyWakeEvent, type Attached, type ToolCall, type ToolEventsOptions, type WakeArgs, type WakeInputs, type WakeKey, type WrappedResult } from "./toolcall.js";
import type { Wakeshift } from "./wakeshift.js";

const CLIENT_INFO_META = "io.modelcontextprotocol/clientInfo";

/** The request context xmcp passes a tool's handler, its second argument: what Wakeshift reads of it. */
export interface XmcpExtra {
  signal?: AbortSignal;
  authInfo?: AuthInfo;
  sessionId?: string;
  _meta?: Record<string, unknown>;
  requestId?: string | number;
  requestInfo?: { headers?: Record<string, string | string[] | undefined> };
  clientInfo?: { name?: string; version?: string };
  [k: string]: any;
}

/** attachXmcp's options. */
export interface XmcpAttachOptions {
  /**
   * Who is behind a request: { user }, your own ID for the person, from your
   * own sign-in (the verified token is extra.authInfo). Without it, requests
   * have no user, so personal events and list_subscriptions refuse.
   */
  identify?(extra: XmcpExtra): Identity | undefined;
  /**
   * Which of attach's own tools attached.tool gives, each `{ enabled }`
   * (default: all three). xmcp can't serve the wait tool. Deprecated: a
   * list of names.
   */
  tools?: Omit<AttachTools, "wait"> | readonly BuiltinTool[];
}

/** One of attach's own tools as an xmcp tool file exports it. */
export interface XmcpTool {
  /** The tool's input, a zod 4 shape: `export const schema = tool.schema`. */
  schema: ZodRawShapeCompat;
  /** Its name, description and annotations: `export const metadata = tool.metadata`. */
  metadata: { name: string; description: string; annotations?: Record<string, unknown> };
  /** Its handler: `export default tool.handler`. */
  handler: (args: Record<string, unknown>, extra: XmcpExtra) => Promise<CallToolResult>;
}

// xmcpReq reads xmcp's handler context: 1.x-shaped (_meta, requestInfo's
// headers, authInfo, signal), with the client's info beside it rather than
// in _meta, where Wakeshift looks for it.
function xmcpReq(raw: unknown): Req {
  const req = v1Req(raw);
  const info = (raw as XmcpExtra | undefined)?.clientInfo;
  if (!info?.name || (req.meta && CLIENT_INFO_META in req.meta)) return req;
  return { ...req, meta: { ...(req.meta ?? {}), [CLIENT_INFO_META]: info } };
}

/** Wakeshift in an xmcp app: what your tool files use. Make it with attachXmcp. */
export class XmcpAttached {
  /** @internal */
  constructor(
    private readonly attached: Attached<XmcpExtra>,
    private readonly own: Map<string, XmcpTool>,
  ) {}

  /**
   * A tool's schema plus Wakeshift's three optional arguments (session_key,
   * harness and conversation_id): `export const schema = attached.inputs({ … })`.
   */
  inputs<S extends ZodRawShapeCompat>(shape: S): S & WakeInputs {
    return this.attached.inputs(shape);
  }

  /**
   * Wraps a tool's handler, for the tool file whose metadata names the same
   * tool: `export default attached.events("watch_ci", async (args, call) => …)`.
   * handler gets your own arguments and a ToolCall, and returns a string or
   * a CallToolResult; the reply gets what Wakeshift adds for the
   * conversation, and a call that fails ends the subscriptions it made. Its
   * schema must come from attached.inputs.
   *
   * subscribes lists the events the tool subscribes to, as attach's
   * attached.events takes it: call.subscribe takes only those. xmcp writes
   * tools/list itself, so give the tool's metadata the same events with
   * attached.describe.
   */
  events<P extends Record<string, any> = Record<string, any>, const E extends readonly AnyWakeEvent[] = readonly AnyWakeEvent[]>(
    name: string,
    opts: ToolEventsOptions<E>,
    handler: (args: P, call: ToolCall<XmcpExtra, E[number]>) => WrappedResult | Promise<WrappedResult>,
  ): (args: P & WakeArgs, extra: XmcpExtra) => Promise<CallToolResult>;
  events<P extends Record<string, any> = Record<string, any>>(
    name: string,
    handler: (args: P, call: ToolCall<XmcpExtra>) => WrappedResult | Promise<WrappedResult>,
  ): (args: P & WakeArgs, extra: XmcpExtra) => Promise<CallToolResult>;
  events(name: string, optsOrHandler: unknown, maybeHandler?: unknown): (args: any, extra: XmcpExtra) => Promise<CallToolResult> {
    type H = (args: Omit<WakeArgs, WakeKey>, call: ToolCall<XmcpExtra>) => WrappedResult | Promise<WrappedResult>;
    const cb =
      typeof optsOrHandler === "function"
        ? this.attached.events<WakeArgs>(name, optsOrHandler as H)
        : this.attached.events<WakeArgs, readonly AnyWakeEvent[]>(name, optsOrHandler as ToolEventsOptions<readonly AnyWakeEvent[]>, maybeHandler as H);
    return async (args, extra) => {
      const out = (await cb(args, extra)) as CallToolResult;
      // xmcp checks structuredContent against the tool's output schema after
      // the handler, refusing fields it doesn't name: the block stays in the
      // content and _meta only.
      const sc = out?.structuredContent;
      if (sc && typeof sc === "object" && !Array.isArray(sc) && WAKESHIFT_FIELD in sc) {
        const { [WAKESHIFT_FIELD]: _block, ...rest } = sc as Record<string, unknown>;
        return { ...out, structuredContent: rest };
      }
      return out;
    };
  }

  /**
   * A subscribing tool's description, for its tool file's metadata: your
   * description, then the sentence attach's tools/list adds for the events
   * the tool declares (their own descriptions: "This tool is how this app
   * notifies you in this conversation of this event: …"). xmcp writes
   * tools/list itself, so the sentence goes in the metadata, naming the
   * app as app ("this app" if not given):
   * `export const metadata = { name: "watch_ci", description: attached.describe("Watch CI on a repo.", { subscribes: [ciFinished], app: "CI bot" }) }`.
   */
  describe(description: string, opts: ToolEventsOptions & { app?: string }): string {
    const list = opts?.subscribes;
    if (!Array.isArray(list)) throw new Error("wakeshift: attached.describe(description, { subscribes }): subscribes lists the events the tool subscribes to");
    return withSubscribes(description, list.map((e) => e.description), opts.app);
  }

  /** One of attach's own tools, for a tool file of its own (src/tools/<name>.ts): its schema, metadata and handler. */
  tool(name: BuiltinTool): XmcpTool {
    const t = this.own.get(name);
    if (!t) {
      throw new Error(
        (BUILTIN_TOOLS as readonly string[]).includes(name)
          ? `wakeshift: attachXmcp's tools leave out ${name}`
          : `wakeshift: attached.tool("${name}"): attach's own tools are ${BUILTIN_TOOLS.join(", ")}`,
      );
    }
    return t;
  }
}

/**
 * Attaches Wakeshift to an xmcp app, after your last defineEvent. Returns
 * what your tool files use: attached.inputs and attached.events for your
 * own tools that subscribe, and attached.tool for attach's own three.
 */
export function attachXmcp(wake: Wakeshift, opts: XmcpAttachOptions = {}): XmcpAttached {
  const events = wake.serveEvents();
  checkAttach(events, opts, "attachXmcp");
  const own = new Map<string, XmcpTool>();
  const host = toolsOnlyHost({
    framework: "xmcp",
    req: xmcpReq,
    registerTool(name, config, cb) {
      const metadata: XmcpTool["metadata"] = { name, description: config.description };
      if (config.annotations) metadata.annotations = config.annotations;
      own.set(name, { schema: config.inputSchema, metadata, handler: (args, extra) => cb(args ?? {}, extra) });
    },
  });
  const attached = attachHost<XmcpExtra>(wake, host, events, { identify: opts.identify, tools: opts.tools });
  return new XmcpAttached(attached, own);
}

export { BUILTIN_TOOLS, type AttachTools, type BuiltinTool, type Identity, type ToolCall, type WakeArgs, type WrappedResult };
