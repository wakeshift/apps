// What attach needs of an MCP server, for each MCP TypeScript SDK it takes:
// @modelcontextprotocol/sdk 1.x and @modelcontextprotocol/server 2.x. The
// rest of the SDK (events.ts, toolcall.ts, tasks.ts, discover.ts) is
// written once against Host, and neither MCP SDK is imported here: hostOf
// tells them apart by the server object it's given.
//
// Where the two differ:
//   - a request's context: 1.x's `extra` (_meta, requestInfo.headers,
//     authInfo, signal), 2.x's `ctx` (mcpReq._meta without the reserved
//     io.modelcontextprotocol/* keys, which it lifts into mcpReq.envelope;
//     http.req's Headers; http.authInfo; mcpReq.signal). req() reads either
//     into one shape, putting the envelope's keys back into _meta, so
//     Wakeshift gets the same call from both (test/mcp-tasks-wire.json);
//   - custom methods (events/*): a zod request schema on 1.x, a method name
//     and a params schema on 2.x;
//   - tasks/get and tasks/cancel: 2.x answers them on protocol 2026-07-28
//     only for a handler registered with a params schema (the tasks
//     extension's methods), so setTaskMethod registers them that way.
//     Before 2.3.0 it refused them there whatever was registered
//     (modelcontextprotocol/typescript-sdk#2598, fixed by #2599), so the
//     peer range starts at 2.3.0 and routesTasks tells attach to warn;
//   - protocol 2026-07-28: 1.x doesn't speak it, so discover.ts teaches it;
//     2.x speaks it itself (server/discover, resultType, ttlMs and
//     cacheScope).
//
// attach also takes either SDK's low-level Server, which has no tool
// registry: lowLevelHost serves attach's own tools from its handler map
// (ToolRegistry), and wraps the tools/call of the tools the app names in
// attach's wrap option (events.ts, toolcall.ts).
//
// Both keep their request handlers in a private Map (_requestHandlers),
// which tools/list, tools/call, server/discover and tasks/* are wrapped
// through (neither has public middleware for a method it answers itself),
// and their tools in _registeredTools, which wrapped tools are checked
// against. Unit tests pin both against each SDK the CI installs.

import { z } from "zod";
import * as z4 from "zod/v4";
import type { AuthInfo, CallToolResult, McpServerLike } from "./mcp.js";
import { UsageError } from "./errors.js";
import { normalizeObjectSchema, objectFromShape, safeParseAsync, toJsonSchema, type AnySchema, type ZodRawShapeCompat } from "./zod.js";

/** @internal A request handler as both SDKs keep them: the request, and the SDK's own context. */
export type Handler = (request: any, raw: any) => Promise<any>;

// ── Wrapping the SDK's request handlers ─────────────────────────────────
//
// Wakeshift works at the protocol layer: it wraps tools/list, tools/call and
// the other methods it amends in the SDK's handler map, never a vendor's
// callback. A handler set again later (a setRequestHandler after attach, as
// an McpServer does on its first registerTool) is wrapped again, so nothing
// a vendor or its middleware re-registers can drop Wakeshift (CR H2).

const WRAPS = Symbol.for("wakeshift.wraps");

/** A wrapping layer for one method: it gets the handler beneath it (undefined: none set). */
export type Layer = (inner: Handler | undefined) => Handler | undefined;
/** A wrapping layer for every method, outside the per-method ones. */
export type OuterLayer = (method: string, inner: Handler) => Handler;

interface Wraps {
  raw: Map<string, Handler | undefined>;
  layers: Map<string, Layer[]>;
  outer: OuterLayer[];
  installed: Map<string, Handler>;
  set: (method: string, h: Handler) => unknown;
}

function wrapsOf(handlers: Map<string, Handler>): Wraps {
  const existing = (handlers as unknown as Record<symbol, Wraps | undefined>)[WRAPS];
  if (existing) return existing;
  const w: Wraps = { raw: new Map(), layers: new Map(), outer: [], installed: new Map(), set: handlers.set.bind(handlers) };
  Object.defineProperty(handlers, WRAPS, { value: w });
  handlers.set = (method: string, h: Handler) => {
    if (!w.layers.has(method) && w.outer.length === 0) return w.set(method, h) as Map<string, Handler>;
    w.raw.set(method, h);
    install(handlers, w, method);
    return handlers;
  };
  return w;
}

// The requests (and their SDK contexts) a wrapped handler is serving. A
// vendor that sets a handler after attach which calls the one it replaced
// (const old = handlers.get("tools/list"); set(…, (r, x) => old(r, x)))
// gets its handler wrapped again, around the wrapped old one: the inner
// wrapping sees the request already marked and runs the handler beneath it
// as it is, so Wakeshift amends a list, or strips a call, once.
const serving = new WeakSet<object>();
const markOf = (v: unknown): object | undefined => (v && typeof v === "object" ? v : undefined);

function install(handlers: Map<string, Handler>, w: Wraps, method: string): void {
  const raw: Handler | undefined = w.raw.get(method);
  let h: Handler | undefined = raw;
  for (const layer of w.layers.get(method) ?? []) h = layer(h);
  if (h) for (const o of w.outer) h = o(method, h);
  if (!h) return;
  const wrapped = h;
  const guarded: Handler = (request, extra) => {
    const marks = [markOf(request), markOf(extra)].filter((m): m is object => !!m);
    if (raw && marks.some((m) => serving.has(m))) return raw(request, extra);
    for (const m of marks) serving.add(m);
    return wrapped(request, extra);
  };
  w.installed.set(method, guarded);
  w.set(method, guarded);
}

/**
 * @internal Wraps method's handler in handlers with layer, now and whenever
 * the handler is set again. Layers apply in the order they're added: the
 * first is innermost.
 */
export function wrapMethod(handlers: Map<string, Handler>, method: string, layer: Layer): void {
  const w = wrapsOf(handlers);
  if (!w.layers.has(method)) {
    w.layers.set(method, []);
    if (!w.raw.has(method)) w.raw.set(method, handlers.get(method));
  }
  w.layers.get(method)!.push(layer);
  install(handlers, w, method);
}

/** @internal Wraps every method's handler, outside the per-method layers, now and whenever one is set. */
export function wrapAll(handlers: Map<string, Handler>, layer: OuterLayer): void {
  const w = wrapsOf(handlers);
  for (const [method, h] of handlers) if (!w.raw.has(method)) w.raw.set(method, h);
  w.outer.push(layer);
  for (const method of [...w.raw.keys()]) install(handlers, w, method);
}

/** @internal Whether method's handler in handlers is still the one the wrapping installed: false if someone replaced it around the map's set. */
export function stillWrapped(handlers: Map<string, Handler>, method: string): boolean {
  const w = (handlers as unknown as Record<symbol, Wraps | undefined>)[WRAPS];
  return !!w && !!w.installed.get(method) && handlers.get(method) === w.installed.get(method);
}

/** @internal One request's context, read the same way from either SDK. */
export interface Req {
  /** The request's _meta, with 2.x's lifted envelope keys put back. */
  meta: Record<string, unknown> | undefined;
  /** An HTTP header of the request ("" if none, or not over HTTP). */
  header(name: string): string;
  /** Every HTTP header of the request, names in lowercase ({} if not over HTTP). */
  headers(): Record<string, string>;
  authInfo: AuthInfo | undefined;
  signal: AbortSignal | undefined;
}

/** @internal A registered tool as the wrapper reads it. */
export interface RegisteredToolInternal {
  inputSchema?: unknown;
  outputSchema?: unknown;
  handler?: unknown;
  callback?: unknown;
}

/** @internal One of attach's own tools' registration. */
export interface ToolRegistration {
  /** Its top-level title, as tools/list gives it. */
  title?: string;
  description: string;
  /** A zod shape, in the API host.zod gives. */
  inputSchema: ZodRawShapeCompat;
  annotations?: Record<string, unknown>;
  /**
   * Its output's JSON Schema, as tools/list gives it (TOOL_OUTPUT_SCHEMAS).
   * An McpServer isn't given it (it takes zod): attach adds it to tools/list.
   */
  outputSchema?: Record<string, unknown>;
}

/** @internal The zod API attach's own tool schemas are written in. */
export type ZodApi = { string(): any; record(k: any, v: any): any; any(): any; enum(values: readonly [string, ...string[]]): any };

/** @internal What attach needs of an MCP server. */
export interface Host {
  /** The MCP SDK's major version: 1 (@modelcontextprotocol/sdk) or 2 (@modelcontextprotocol/server). */
  readonly version: 1 | 2;
  /** The framework adapter serving it ("mastra", "xmcp", "low-level Server"), or undefined for attach on an McpServer. */
  readonly framework?: string;
  /** For a server with no tool registry (a low-level Server, a framework's): where Wakeshift's own tools are served from. */
  readonly registry?: ToolRegistry;
  /** Whether the server serves MCP Events (events/*): only then does a wrapped call have native callers. */
  readonly servesEvents: boolean;
  readonly server: McpServerLike;
  /** The zod attach's own tools are written with: zod 4 for 2.x, which takes only zod 4. */
  readonly zod: ZodApi;
  /** A request's context, from the SDK's own (extra or ctx), or one already read. */
  req(raw: unknown): Req;
  registerCapabilities(capabilities: Record<string, unknown>): void;
  registerTool(name: string, config: ToolRegistration, cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>): { enable(): void; disable(): void };
  /** A JSON-RPC method of Wakeshift's own (events/*): its handler gets the request's params. */
  setMethod(method: string, handler: (params: Record<string, any>, raw: unknown) => Promise<unknown>): void;
  /** Makes the SDK route tasks/get or tasks/cancel on every protocol revision; the caller then sets its handler in handlers(). */
  setTaskMethod(method: string): void;
  /**
   * Whether the SDK passes tasks/get and tasks/cancel on protocol
   * 2026-07-28 to the handlers setTaskMethod registered: false for
   * @modelcontextprotocol/server before TASKS_MIN_SERVER, which answers
   * them -32601 itself.
   */
  routesTasks(): boolean;
  /** The SDK's request handlers, or null if this SDK keeps them where Wakeshift can't wrap them. */
  handlers(): Map<string, Handler> | null;
  /** The client's name from initialize (or, on 2.x, the request's envelope). */
  clientName(): string | undefined;
  /** The client's capabilities from initialize (or, on 2.x, the request's envelope). */
  clientCapabilities(): Record<string, unknown> | undefined;
  /** The server's own capabilities, as it registered them. */
  serverCapabilities(): Record<string, unknown> | undefined;
  /** Runs fn after a client's initialized notification, after any earlier hook. */
  onInitialized(fn: () => void): void;
  /** On a server already initialized by its client (fastmcp's connect over stdio), tells the client its tools changed, if the server declares tools.listChanged. */
  toolsChangedIfLate?(): void;
  /** A registered tool (undefined: none by that name; null: the SDK keeps them where Wakeshift can't read them). */
  registeredTool(name: string): RegisteredToolInternal | undefined | null;
  /** Whether a tools/call result passes the tool's output schema, as the SDK checks it after the callback returns. */
  validOutput(outputSchema: unknown, result: CallToolResult | string): Promise<boolean>;
}

const VERSION_META = "io.modelcontextprotocol/protocolVersion";

/**
 * @internal The oldest @modelcontextprotocol/server that serves tasks/get
 * and tasks/cancel on protocol 2026-07-28 (modelcontextprotocol/typescript-sdk#2599):
 * the floor of the SDK's peer range for it.
 */
export const TASKS_MIN_SERVER = "2.3.0";

function lowOf(server: McpServerLike): any {
  return (server as { server?: unknown }).server;
}

/**
 * @internal The Host for an McpServer, or a low-level Server, from either
 * SDK. 2.x's low-level Server has projectCallToolResult, which 1.x's lacks.
 */
export function hostOf(server: unknown): Host {
  if (isLowServer(server)) return lowLevelHost(server);
  if (!isMcpServer(server)) throw new UsageError("config", unsupportedServer(server));
  const low = (server as { server: any }).server;
  return typeof low.projectCallToolResult === "function" ? new V2Host(server as McpServerLike) : new V1Host(server as McpServerLike);
}

// The servers attach was given, and the low-level servers inside the
// McpServers among them: a second attach, or attaching an McpServer's inner
// server, is refused (CR M1, M3).
const attachedServers = new WeakSet<object>();
const innerServers = new WeakSet<object>();

/** @internal Refuses a server attach was already called on, or an McpServer's inner server; then remembers it. */
export function claimServer(server: unknown): void {
  if (!server || typeof server !== "object") return; // hostOf says what it got
  if (attachedServers.has(server)) {
    throw new UsageError("config", "attach was already called on this server: attach once per server (a server built per request gets its own attach)");
  }
  if (innerServers.has(server)) throw new UsageError("config", "attach got an McpServer's inner .server: pass the McpServer itself");
  attachedServers.add(server);
  if (isMcpServer(server)) innerServers.add((server as { server: object }).server);
}

/** @internal Whether server is a low-level Server from either MCP SDK (setRequestHandler and connect, no registerTool). */
export function isLowServer(server: unknown): boolean {
  return has(server, "setRequestHandler", "connect") && !has(server, "registerTool");
}

/** What a low-level Server's host calls itself in warnings and errors. */
export const LOW_LEVEL = "low-level Server";

/** Where the docs list the MCP server frameworks Wakeshift works with, and how. */
export const FRAMEWORKS_DOCS = "https://wakeshift.ai/docs/frameworks";

// isMcpServer: an McpServer from either MCP SDK (its low-level server, and registerTool).
function isMcpServer(server: unknown): boolean {
  const low = (server as { server?: any } | null | undefined)?.server;
  return !!low && typeof low.setRequestHandler === "function" && typeof (server as { registerTool?: unknown }).registerTool === "function";
}

const TAKES = "attach takes an McpServer or a low-level Server, from @modelcontextprotocol/server 2.x or @modelcontextprotocol/sdk 1.x";

/**
 * @internal What attach says when it's given something other than an
 * McpServer: what it got, what it takes, and, for a framework it knows,
 * what to do instead.
 */
export function unsupportedServer(server: unknown): string {
  const got = describe(server);
  const hint = frameworkHint(server);
  return `${TAKES}, and got ${got}.${hint ? " " + hint : ""} Frameworks Wakeshift works with, and how: ${FRAMEWORKS_DOCS}`;
}

function has(o: unknown, ...fns: string[]): boolean {
  return !!o && (typeof o === "object" || typeof o === "function") && fns.every((f) => typeof (o as Record<string, unknown>)[f] === "function");
}

function ctorName(o: unknown): string {
  const name = (o as { constructor?: { name?: unknown } } | null)?.constructor?.name;
  return typeof name === "string" && name && name !== "Object" ? name : "";
}

// describe names what attach was given: its class, else its type.
function describe(server: unknown): string {
  if (server === null) return "null";
  if (server === undefined) return "undefined";
  if (typeof server !== "object") return `a ${typeof server}`;
  const name = ctorName(server);
  return name ? `a ${name}` : "an object";
}

// frameworkHint says what to do for a server from a framework attach knows
// but can't take: "" for any other.
function frameworkHint(server: unknown): string {
  if (!server || typeof server !== "object") return "";
  // Mastra's MCPServer (@mastra/mcp): builds its MCP server per request, inside.
  if (has(server, "startHTTP", "convertTools") || (has(server, "startHTTP", "startStdio") && "toolActions" in server)) {
    return "This looks like Mastra's MCPServer (@mastra/mcp), which builds its MCP server inside: use attachMastra from @wakeshift/sdk/mastra.";
  }
  // punkpeye's fastmcp: its FastMCP, or a session of it.
  if (has(server, "addTool", "start") && ctorName(server) === "FastMCP") {
    return "This looks like fastmcp's FastMCP (punkpeye): attach each session's server instead, server.on(\"connect\", ({ session }) => wake.attach(session.server, { wrap })), and serve HTTP with server.connect(transport), since fastmcp's start() refuses MCP protocol 2026-07-28.";
  }
  if (ctorName(server) === "FastMCPSession") {
    return "This looks like a fastmcp session (punkpeye): pass its server, wake.attach(session.server, { wrap }).";
  }
  // Cloudflare's McpAgent: its server is the McpServer.
  if ("server" in server && has(server, "init") && isMcpServer((server as { server: unknown }).server)) {
    return "This looks like a Cloudflare McpAgent: pass its McpServer (this.server).";
  }
  return "";
}

/**
 * @internal attach's error when the server refused Wakeshift's
 * capabilities: an McpServer already connected to a transport, or
 * whatever else its MCP SDK said.
 */
export function capabilitiesError(host: Host, err: unknown): UsageError {
  const low = (host.server as { server?: { transport?: unknown } }).server;
  if (low?.transport) return new UsageError("config", "call attach before connecting the server to a transport", { cause: err });
  return new UsageError("config", `attach couldn't add Wakeshift's capabilities to this server: ${String((err as Error)?.message ?? err)}`, { cause: err });
}

// headerOf reads a header from a plain record (1.x's requestInfo.headers).
function headerOf(headers: Record<string, string | string[] | undefined> | undefined, name: string): string {
  if (!headers) return "";
  const v = headers[name] ?? headers[name.toLowerCase()];
  return (Array.isArray(v) ? v[0] : v) ?? "";
}

// headerRecord is a plain record's headers, names in lowercase, a list's first value.
function headerRecord(headers: Record<string, string | string[] | undefined> | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(headers ?? {})) {
    const one = Array.isArray(v) ? v[0] : v;
    if (typeof one === "string") out[k.toLowerCase()] = one;
  }
  return out;
}

/** @internal A context already read (req's output). */
function isReq(raw: unknown): raw is Req {
  return !!raw && typeof (raw as Req).header === "function" && "meta" in (raw as object);
}

/**
 * @internal A request's context read the 1.x way: extra's _meta,
 * requestInfo.headers, authInfo and signal. Also what a plain
 * `{ _meta, requestInfo }` (ToolExtra) reads as.
 */
export function v1Req(raw: unknown): Req {
  if (isReq(raw)) return raw;
  const e = (raw ?? {}) as { _meta?: Record<string, unknown>; requestInfo?: { headers?: Record<string, string | string[] | undefined> }; authInfo?: AuthInfo; signal?: AbortSignal };
  return { meta: e._meta, header: (name) => headerOf(e.requestInfo?.headers, name), headers: () => headerRecord(e.requestInfo?.headers), authInfo: e.authInfo, signal: e.signal };
}

/** @internal A request's context read the 2.x way (ctx), putting the envelope's keys back into _meta. */
export function v2Req(raw: unknown): Req {
  if (isReq(raw)) return raw;
  const c = (raw ?? {}) as {
    mcpReq?: { _meta?: Record<string, unknown>; envelope?: Record<string, unknown>; signal?: AbortSignal };
    http?: { authInfo?: AuthInfo; req?: { headers?: { get(name: string): string | null } } };
    // A 1.x-shaped context (a test's, or one passed through by hand) reads too.
    _meta?: Record<string, unknown>;
    requestInfo?: { headers?: Record<string, string | string[] | undefined> };
  };
  if (!c.mcpReq) return v1Req(raw);
  const own = c.mcpReq._meta;
  const env = c.mcpReq.envelope;
  const meta = env && Object.keys(env).length > 0 ? { ...env, ...own } : own;
  const headers = c.http?.req?.headers;
  return {
    meta,
    header: (name) => (headers && typeof headers.get === "function" ? (headers.get(name) ?? "") : ""),
    headers: () => {
      const out: Record<string, string> = {};
      const h = headers as unknown as { forEach?: (fn: (v: string, k: string) => void) => void } | undefined;
      h?.forEach?.((v, k) => (out[k.toLowerCase()] = v));
      return out;
    },
    authInfo: c.http?.authInfo,
    signal: c.mcpReq.signal,
  };
}

abstract class BaseHost {
  readonly servesEvents: boolean = true;
  constructor(readonly server: McpServerLike) {}
  protected get low(): any {
    return lowOf(this.server);
  }
  registerCapabilities(capabilities: Record<string, unknown>): void {
    this.low.registerCapabilities(capabilities);
  }
  handlers(): Map<string, Handler> | null {
    const h = this.low._requestHandlers;
    return h instanceof Map ? (h as Map<string, Handler>) : null;
  }
  clientName(): string | undefined {
    return this.low.getClientVersion?.()?.name || undefined;
  }
  clientCapabilities(): Record<string, unknown> | undefined {
    return this.low.getClientCapabilities?.() as Record<string, unknown> | undefined;
  }
  toolsChangedIfLate(): void {
    try {
      if (!this.low.transport || !this.clientCapabilities()) return;
      const tools = (this.serverCapabilities() as { tools?: { listChanged?: boolean } } | undefined)?.tools;
      if (!tools?.listChanged || typeof this.low.sendToolListChanged !== "function") return;
      void Promise.resolve(this.low.sendToolListChanged()).catch(() => undefined);
    } catch {
      // Nothing to tell: the client lists the tools as they are when it next asks.
    }
  }
  serverCapabilities(): Record<string, unknown> | undefined {
    try {
      return this.low.getCapabilities?.() as Record<string, unknown> | undefined;
    } catch {
      return undefined;
    }
  }
  onInitialized(fn: () => void): void {
    const low = this.low;
    const prev = low.oninitialized as (() => void) | undefined;
    low.oninitialized = () => {
      fn();
      prev?.();
    };
  }
  registeredTool(name: string): RegisteredToolInternal | undefined | null {
    const tools = (this.server as unknown as { _registeredTools?: Record<string, RegisteredToolInternal> })._registeredTools;
    if (!tools || typeof tools !== "object") return null;
    return Object.prototype.hasOwnProperty.call(tools, name) ? tools[name] : undefined;
  }
  // A vendor's tool registered before attach under one of Wakeshift's names:
  // Wakeshift's is served from the handler map instead, and wins (CR L1).
  protected taken(name: string): boolean {
    return !!this.registeredTool(name);
  }
  private conflicts?: ToolRegistry;
  /** Called with a vendor tool named like one of Wakeshift's (Wakeshift's wins). */
  onConflict?: (name: string) => void;
  protected conflictRegistry(): ToolRegistry {
    if (!this.conflicts) {
      this.conflicts = new ToolRegistry(this.low);
      this.conflicts.onConflict = (name) => this.onConflict?.(name);
    }
    return this.conflicts;
  }
}

class V1Host extends BaseHost implements Host {
  readonly version = 1 as const;
  readonly zod: ZodApi = z as unknown as ZodApi;
  req(raw: unknown): Req {
    return v1Req(raw);
  }
  registerTool(name: string, config: ToolRegistration, cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>) {
    if (this.taken(name)) return this.conflictRegistry().registerTool(name, { ...config, inputSchema: objectFromShape(config.inputSchema) as AnySchema }, cb);
    const { outputSchema: _json, ...forMcp } = config;
    return (this.server as any).registerTool(name, forMcp, cb);
  }
  setMethod(method: string, handler: (params: Record<string, any>, raw: unknown) => Promise<unknown>): void {
    const schema = z.object({ method: z.literal(method), params: z.any().optional() });
    this.low.setRequestHandler(schema, (request: { params?: Record<string, any> }, extra: unknown) => handler(request.params ?? {}, extra));
  }
  setTaskMethod(_method: string): void {
    // Nothing to register: 1.x routes whatever is in its handler map. (Its
    // setRequestHandler would ask for its own tasks capability, an older
    // revision's, never advertised.)
  }
  routesTasks(): boolean {
    return true;
  }
  async validOutput(outputSchema: unknown, result: CallToolResult | string): Promise<boolean> {
    // McpServer 1.x's validateToolOutput: an object schema, and structured content that parses.
    if (!outputSchema) return true;
    if (typeof result === "string" || !result.structuredContent) return false;
    const obj = normalizeObjectSchema(outputSchema);
    if (!obj) return true;
    return (await safeParseAsync(obj as AnySchema, result.structuredContent)).success;
  }
}

class V2Host extends BaseHost implements Host {
  readonly version = 2 as const;
  readonly zod: ZodApi = z4 as unknown as ZodApi;
  req(raw: unknown): Req {
    return v2Req(raw);
  }
  registerTool(name: string, config: ToolRegistration, cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>) {
    // 2.x takes an object schema (a bare shape is deprecated there).
    const { outputSchema: _json, ...forMcp } = config;
    if (!(this.server instanceof ToolRegistry) && this.taken(name)) return this.conflictRegistry().registerTool(name, { ...config, inputSchema: objectFromV4Shape(config.inputSchema) as AnySchema }, cb);
    if (this.server instanceof ToolRegistry) return this.server.registerTool(name, { ...config, inputSchema: objectFromV4Shape(config.inputSchema) as z4.ZodObject }, cb);
    return (this.server as any).registerTool(name, { ...forMcp, inputSchema: objectFromV4Shape(config.inputSchema) }, cb);
  }
  setMethod(method: string, handler: (params: Record<string, any>, raw: unknown) => Promise<unknown>): void {
    this.low.setRequestHandler(method, { params: z4.looseObject({}) }, (params: Record<string, any>, ctx: unknown) => handler(params ?? {}, ctx));
  }
  setTaskMethod(method: string): void {
    // Registered with a params schema, so 2.x serves it on protocol
    // 2026-07-28 too, as the tasks extension's method (SEP-2663). The
    // caller replaces the handler that leaves in the map.
    this.low.setRequestHandler(method, { params: z4.looseObject({}) }, async () => {
      throw Object.assign(new Error("Method not found"), { code: -32601 });
    });
  }
  routesTasks(): boolean {
    // 2.3.0's fix keeps the methods registered with a schema in this set,
    // and lets them past the era gate; 2.0 to 2.2 have no such set. 2.x
    // doesn't expose its version (nor its package.json), so the fix is
    // what's checked, private like _requestHandlers. A release that renames
    // it would only warn wrongly, and fails v2.test.ts's check that the
    // installed 2.x doesn't warn, on the Dependabot bump that brings it.
    return this.low._customSchemaRequestMethods instanceof Set;
  }
  async validOutput(outputSchema: unknown, result: CallToolResult | string): Promise<boolean> {
    // McpServer 2.x's validateToolOutput: structured content (any JSON
    // value) that the output schema (a Standard Schema) accepts.
    if (!outputSchema) return true;
    if (typeof result === "string" || result.structuredContent === undefined) return false;
    const std = (outputSchema as { "~standard"?: { validate(v: unknown): unknown } })["~standard"];
    if (!std || typeof std.validate !== "function") return true;
    const out = (await std.validate(result.structuredContent)) as { issues?: unknown };
    return !out.issues;
  }
}

// objectFromV4Shape is a zod 4 object of attach's own tool shapes (written
// with zod/v4: host.zod).
function objectFromV4Shape(shape: ZodRawShapeCompat): unknown {
  const values = Object.values(shape);
  if (values.length === 0) return z4.object({});
  return z4.object(shape as Record<string, z4.ZodType>);
}

// ── Framework adapters (mastra.ts, xmcp.ts) ─────────────────────────────

/**
 * @internal The Host for a low-level Server from @modelcontextprotocol/server
 * 2.x, as a framework builds one for each request and fills in itself
 * (Mastra's MCPServer). It has no McpServer to register attach's own
 * tools on, so they're kept here and served from its handler map, beneath
 * every other wrapper attach adds: tools/list lists them after the
 * framework's own, and tools/call answers them. Everything else is
 * V2Host's, on the same map.
 */
export function lowServerHost(low: any, framework: string): Host {
  if (!low || typeof low.setRequestHandler !== "function" || !(low._requestHandlers instanceof Map)) {
    throw new UsageError("config", `${framework} built an MCP server Wakeshift can't serve (no request handlers it can wrap): check for a newer @wakeshift/sdk`);
  }
  return new LowV2Host(new ToolRegistry(low), framework);
}

interface OwnTool {
  title?: string;
  description: string;
  annotations?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
  schema: AnySchema;
  cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>;
  enabled: boolean;
}

// ToolRegistry stands in for an McpServer around a low-level Server: what
// attach registers on it is listed and called through the server's handlers,
// as they are when attach is called (a server with neither gets both, with
// the tools capability): set your own before attach.
/** @internal Wakeshift's own tools on a server with no tool registry, served from its handler map. */
export class ToolRegistry {
  readonly tools = new Map<string, OwnTool>();
  constructor(readonly server: any) {
    const handlers = server._requestHandlers as Map<string, Handler>;
    if (!handlers.has("tools/list") || !handlers.has("tools/call")) {
      try {
        server.registerCapabilities({ tools: {} });
      } catch {
        // connected already: attach says so (capabilitiesError)
      }
    }
    // Wakeshift's own tools after the server's, on its first page only;
    // one of the server's own named like one of them is replaced (Wakeshift's wins).
    wrapMethod(handlers, "tools/list", (list) => async (request: any, raw: any) => {
      const out = list ? await list(request, raw) : { tools: [] };
      const own = [...this.tools].filter(([, t]) => t.enabled);
      if (own.length === 0 || request?.params?.cursor !== undefined || !out || !Array.isArray(out.tools)) return out;
      const ours = new Set(own.map(([name]) => name));
      const theirs = out.tools.filter((t: { name?: unknown }) => !ours.has(t?.name as string));
      if (theirs.length !== out.tools.length) for (const t of out.tools) if (ours.has(t?.name)) this.onConflict?.(t.name);
      return { ...out, tools: [...theirs, ...own.map(([name, t]) => listedTool(name, t))] };
    });
    wrapMethod(handlers, "tools/call", (call) => async (request: any, raw: any) => {
      const t = this.tools.get(request?.params?.name);
      if (!t || !t.enabled) {
        if (call) return call(request, raw);
        throw Object.assign(new Error(`Unknown tool: ${request?.params?.name}`), { code: -32602 });
      }
      const parsed = await safeParseAsync(t.schema, request?.params?.arguments ?? {});
      if (!parsed.success) {
        // As the MCP SDK's McpServer answers it.
        const said = (parsed.error as { message?: unknown })?.message;
        return { isError: true, content: [{ type: "text", text: `Input validation error: Invalid arguments for tool ${request.params.name}: ${typeof said === "string" ? said : "invalid"}` }] };
      }
      return t.cb(parsed.data as Record<string, unknown>, raw);
    });
  }
  /** Called with a tool of the server's own named like one of Wakeshift's (Wakeshift's wins). */
  onConflict?: (name: string) => void;
  registerTool(name: string, config: { title?: string; description: string; inputSchema: AnySchema; annotations?: Record<string, unknown>; outputSchema?: Record<string, unknown> }, cb: OwnTool["cb"]) {
    const t: OwnTool = { title: config.title, description: config.description, annotations: config.annotations, outputSchema: config.outputSchema, schema: config.inputSchema, cb, enabled: true };
    this.tools.set(name, t);
    return {
      enable: () => void (t.enabled = true),
      disable: () => void (t.enabled = false),
    };
  }
}

function listedTool(name: string, t: OwnTool): Record<string, unknown> {
  const json = toJsonSchema(t.schema);
  delete json.$schema;
  const out: Record<string, unknown> = { name, ...(t.title ? { title: t.title } : {}), description: t.description, inputSchema: json };
  if (t.annotations) out.annotations = t.annotations;
  if (t.outputSchema) out.outputSchema = t.outputSchema;
  return out;
}

class LowV2Host extends V2Host {
  constructor(
    readonly registry: ToolRegistry,
    readonly framework: string,
  ) {
    super(registry as unknown as McpServerLike);
  }
  registeredTool(): null {
    return null; // the framework's own tools are its own
  }
}

// LowV1Host is a low-level Server from @modelcontextprotocol/sdk 1.x:
// V1Host's methods on its handler map, with attach's own tools served from
// it (ToolRegistry), written in zod 4 as the registry takes them.
class LowV1Host extends V1Host {
  readonly framework = LOW_LEVEL;
  readonly zod: ZodApi = z4 as unknown as ZodApi;
  constructor(readonly registry: ToolRegistry) {
    super(registry as unknown as McpServerLike);
  }
  registerTool(name: string, config: ToolRegistration, cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>) {
    return this.registry.registerTool(name, { ...config, inputSchema: objectFromV4Shape(config.inputSchema) as z4.ZodObject }, cb);
  }
  registeredTool(): null {
    return null; // the server's tools are its own: it has no registry
  }
  // A server connected to its transport but not yet initialized (one built
  // per request and handed out connected, as fastmcp's "connect" event
  // does): 1.x's registerCapabilities refuses once connected, but nothing
  // has been answered yet, so the capabilities merge as it would merge them.
  registerCapabilities(capabilities: Record<string, unknown>): void {
    const low = this.low;
    if (low.transport && !low.getClientCapabilities?.() && isPlainObject(low._capabilities)) {
      const out: Record<string, unknown> = { ...low._capabilities };
      for (const [k, v] of Object.entries(capabilities)) {
        const base = out[k];
        out[k] = isPlainObject(base) && isPlainObject(v) ? { ...base, ...v } : v;
      }
      low._capabilities = out;
      return;
    }
    low.registerCapabilities(capabilities);
  }
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === "object" && !Array.isArray(v);
}

/**
 * @internal The Host for a low-level Server an app built and passed to
 * attach, from @modelcontextprotocol/sdk 1.x or @modelcontextprotocol/server
 * 2.x: attach's own tools are served from its handler map (ToolRegistry),
 * and the calls of the tools the app names in attach's wrap option are
 * wrapped there (aroundCalls).
 */
export function lowLevelHost(low: any): Host {
  if (!(low._requestHandlers instanceof Map)) {
    throw new UsageError("config", `this ${LOW_LEVEL} keeps its request handlers where Wakeshift can't wrap them: check for a newer @wakeshift/sdk`);
  }
  const registry = new ToolRegistry(low);
  return typeof low.projectCallToolResult === "function" ? new LowV2Host(registry, LOW_LEVEL) : new LowV1Host(registry);
}

/**
 * @internal The Host for a runtime with no MCP server at all (attachTools):
 * an in-memory handler map, through which Wakeshift's own tools and its
 * tools/list and tools/call layers run, exactly as on a low-level Server.
 * There's no client to negotiate with, so no MCP Events (servesEvents is
 * false) and no wait tool.
 */
export function toolsHost(req: (raw: unknown) => Req): { host: Host; handlers: Map<string, Handler> } {
  const handlers = new Map<string, Handler>();
  const low = {
    _requestHandlers: handlers,
    registerCapabilities() {},
    setRequestHandler(method: string, _schemas: unknown, handler: (params: unknown, ctx: unknown) => Promise<unknown>) {
      handlers.set(method, (request: any, ctx: any) => handler(request?.params ?? {}, ctx));
    },
  };
  class ToolsHost extends LowV2Host {
    readonly servesEvents = false;
    req(raw: unknown): Req {
      return req(raw);
    }
  }
  return { host: new ToolsHost(new ToolRegistry(low), "tools"), handlers };
}

/** @internal How a framework adapter that has no MCP server (xmcp's) reads a request and registers attach's own tools. */
export interface ToolsOnlyOptions {
  framework: string;
  /** A tool callback's request context, as the framework passes it. */
  req(raw: unknown): Req;
  /** Where attach's own tools go: the adapter serves them its framework's way. */
  registerTool(name: string, config: ToolRegistration, cb: (args: Record<string, unknown>, raw: unknown) => Promise<CallToolResult>): void;
}

/**
 * @internal The Host for a framework that keeps its MCP server to itself
 * (xmcp): attach's own tools go to the adapter, the request is read its
 * way, and there are no handlers to wrap, so no MCP Events, no wait tool
 * and no tools/list changes. Its tools are on @modelcontextprotocol/server
 * 2.x, so they're written in zod 4.
 */
export function toolsOnlyHost(opts: ToolsOnlyOptions): Host {
  const noop = () => {};
  return {
    version: 2,
    framework: opts.framework,
    servesEvents: false,
    server: { server: {}, registerTool: noop } as unknown as McpServerLike,
    zod: z4 as unknown as ZodApi,
    req: opts.req,
    registerCapabilities: noop,
    registerTool(name, config, cb) {
      opts.registerTool(name, config, cb);
      return { enable: noop, disable: noop };
    },
    setMethod: noop,
    setTaskMethod: noop,
    routesTasks: () => true,
    handlers: () => null,
    clientName: () => undefined,
    clientCapabilities: () => undefined,
    serverCapabilities: () => undefined,
    onInitialized: noop,
    registeredTool: () => null,
    validOutput: async () => true,
  };
}

/** The protocol revision that has server/discover, MCP Events and the tasks extension. */
export const STATELESS_VERSION = "2026-07-28";

/** @internal Whether a request is protocol 2026-07-28: its _meta names it, or its MCP-Protocol-Version header does. */
export function is2026(req: Req): boolean {
  if (req.meta?.[VERSION_META] === STATELESS_VERSION) return true;
  return req.header("mcp-protocol-version") === STATELESS_VERSION;
}
