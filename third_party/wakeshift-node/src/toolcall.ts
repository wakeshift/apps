// One way to subscribe from a tool (docs/subscribe-from-tools.md): a tool
// callback wrapped by the handle attach returns (attached.events) gets a
// ToolCall, and subscribes the calling conversation with call.subscribe
// (ends it with call.unsubscribe). The wrapper adds one Wakeshift block to
// whatever the handler returns, so a conversation can't be left unwakeable
// by a forgotten argument, a wrong tool name or an unreturned result; and a
// call that fails ends the subscriptions it made. A port of the Go SDK's
// appkit.Events (pkg/appkit/toolcall.go, attachcall.go).
//
//   const attached = wake.attach(server, { identify: (extra) => ({ user: userOf(extra) }) })
//   server.registerTool("join",
//     { description: "…", inputSchema: attached.inputs({ table: z.string() }) },
//     attached.events("join", async ({ table }, call) => {
//       const sub = await call.subscribe(turn, { table }, { reason: `Playing at ${table}` })
//       await casino.seat(call.user!, table, sub.id)
//       return `You're seated at ${table}.`
//     }))

import { z } from "zod";
import * as z3 from "zod/v3";
import * as z4 from "zod/v4";
import type { CallToolResult, ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import type { McpServer, ToolCallback } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import { getObjectShape, isZ4Schema, normalizeObjectSchema, safeParseAsync, type AnySchema, type SchemaInput, type ShapeOutput, type ZodRawShapeCompat } from "@modelcontextprotocol/sdk/server/zod-compat.js";
import { WakeshiftError } from "./errors.js";
import { EXTENSION_KEY, dialectOf } from "./spec.js";
import type { Identity, WakeEvent } from "./events.js";
import { packCall, type Call, type Duration, type Recognized, type Wakeshift } from "./wakeshift.js";

// Wakeshift's three arguments (wakeshift.ts's SESSION_ARG, HARNESS_ARG and
// CONVERSATION_ARG, repeated: that module imports this one).
const SESSION_ARG = "session_key";
const HARNESS_ARG = "harness";
const CONVERSATION_ARG = "conversation_id";

/** MCP's own extra for a tool call: the request's signal, auth, _meta, HTTP request. */
export type ToolCallExtra = RequestHandlerExtra<ServerRequest, ServerNotification>;

// What the agent is told each of Wakeshift's arguments is: the Go SDK's
// WakeArgs, word for word.
const SESSION_KEY_DESCRIPTION =
  "This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it.";
const HARNESS_DESCRIPTION =
  "The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed";
const CONVERSATION_DESCRIPTION =
  "Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty";
const WAKESHIFT_OUTPUT_DESCRIPTION = "What Wakeshift adds for this conversation: how it's woken by this app's events";

const wakeInputs = {
  [SESSION_ARG]: z.string().optional().describe(SESSION_KEY_DESCRIPTION),
  [HARNESS_ARG]: z.string().optional().describe(HARNESS_DESCRIPTION),
  [CONVERSATION_ARG]: z.string().optional().describe(CONVERSATION_DESCRIPTION),
};
/** @internal Wakeshift's three arguments as attach's own tools take them: worded as a wrapped tool's (test/events-list.json pins it). */
export const toolArgs = wakeInputs;
const wakeOutput = { wakeshift: z.string().optional().describe(WAKESHIFT_OUTPUT_DESCRIPTION) };

// The same in each of zod's two APIs: MCP refuses a shape that mixes them,
// so a shape written with zod/v3 (or zod/v4) gets Wakeshift's fields in its
// own (zod 3.25 and later carry both).
const byVersion = {
  v3: {
    inputs: {
      [SESSION_ARG]: z3.string().optional().describe(SESSION_KEY_DESCRIPTION),
      [HARNESS_ARG]: z3.string().optional().describe(HARNESS_DESCRIPTION),
      [CONVERSATION_ARG]: z3.string().optional().describe(CONVERSATION_DESCRIPTION),
    },
    output: { wakeshift: z3.string().optional().describe(WAKESHIFT_OUTPUT_DESCRIPTION) },
  },
  v4: {
    inputs: {
      [SESSION_ARG]: z4.string().optional().describe(SESSION_KEY_DESCRIPTION),
      [HARNESS_ARG]: z4.string().optional().describe(HARNESS_DESCRIPTION),
      [CONVERSATION_ARG]: z4.string().optional().describe(CONVERSATION_DESCRIPTION),
    },
    output: { wakeshift: z4.string().optional().describe(WAKESHIFT_OUTPUT_DESCRIPTION) },
  },
};

// fieldsFor is Wakeshift's fields in the zod API shape's own fields use
// (the default import's for an empty shape).
function fieldsFor<K extends "inputs" | "output">(shape: ZodRawShapeCompat, kind: K): K extends "inputs" ? WakeInputs : WakeOutputShape {
  const first = Object.values(shape ?? {})[0];
  const fields = !first ? (kind === "inputs" ? wakeInputs : wakeOutput) : byVersion[isZ4Schema(first) ? "v4" : "v3"][kind];
  return fields as any;
}

/** Wakeshift's three arguments, as attached.inputs adds them. */
export type WakeInputs = typeof wakeInputs;
/** The optional wakeshift string attached.output adds. */
export type WakeOutputShape = typeof wakeOutput;
/** The names of Wakeshift's three arguments. */
export type WakeKey = typeof SESSION_ARG | typeof HARNESS_ARG | typeof CONVERSATION_ARG;
/** An input shape with Wakeshift's three arguments in it (attached.inputs'). */
export type WakeShape = ZodRawShapeCompat & { [K in WakeKey]: AnySchema };
/** What a wrapped handler gets: the tool's own arguments, without Wakeshift's three. */
export type AppArgs<S extends ZodRawShapeCompat> = Omit<ShapeOutput<S>, WakeKey>;
/** What a wrapped handler returns: a string (one text block) or a CallToolResult. */
export type WrappedResult = string | CallToolResult;

const WAKE_KEYS: readonly WakeKey[] = [SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG];

/**
 * @internal The tools/call _meta keys a harness names its conversation in
 * (the core's harnesses ConversationMeta, dotted for nested objects):
 * test/conversation-meta.json, which a Go test keeps current.
 */
export const CONVERSATION_META: readonly string[] = [
  "agent-session-id",
  "antigravity.google/conversation_id",
  "vscode.conversationId",
  "wakeshift/conversation",
  "x-codex-turn-metadata.thread_id",
];

/**
 * The calling conversation's subscription, made or renewed by
 * call.subscribe. id and user address it later: emit's
 * `to: [{ user, subscription: id }]`, or `event.end(user, id)`.
 */
export interface Subscription {
  id: string;
  user: string;
  /** False for a renewal. */
  created: boolean;
  /** The conversation's session. A secret: don't show, store or log it. */
  session: string;
  /** What Wakeshift recognized the agent's program as. */
  recognized: Recognized;
  /** The caller subscribes itself (call.native): nothing was made, id is "", and the block tells it what to subscribe to. */
  native: boolean;
}

/** How call.subscribe subscribes. */
export interface ToolSubscribeOptions {
  /** End after this long without a delivery or a fresh subscribe (default 30 days). */
  ttl?: Duration;
  /** Why the app subscribes the conversation, shown in the console (at most 200 characters). */
  reason?: string;
  /** Names a new session (agent@label), for an app acting for its agents. */
  label?: string;
}

/**
 * One call of a wrapped tool (attached.events): who's calling, and the
 * calls that subscribe the calling conversation. Only the wrapper makes
 * one, and it works until the handler returns (then CallEnded).
 */
export interface ToolCall {
  /** Who attach's identify says is calling (undefined for none). */
  readonly user: string | undefined;
  /** The tool being called. */
  readonly tool: string;
  /**
   * The client subscribes to the app's events itself (MCP Events: ChatGPT,
   * the house agents). call.subscribe then makes no conversation
   * subscription, and the block tells it what to subscribe to.
   */
  readonly native: boolean;
  /** A native client's conversation, when it names one (ChatGPT's _meta["openai/session"]). */
  readonly nativeConversation: string | null;
  /** The session_key passed; after a subscribe or resolveSession, the conversation's session. */
  readonly session: string | null;
  /** What Wakeshift recognized the agent's program as: null until a subscribe or resolveSession. */
  readonly recognized: Recognized | null;
  /** MCP's own extra for the call. */
  readonly extra: ToolCallExtra;
  /**
   * Subscribes the calling conversation to event with args, or renews its
   * subscription. The reply then carries what the conversation needs to be
   * woken (one block for the call, however many subscribes it makes), and
   * if the call fails, the subscription is ended. For a native caller
   * nothing is made. Your tool decides who may: authorize isn't consulted.
   * A personal event needs a user (identify's).
   */
  subscribe<A extends AnySchema>(event: WakeEvent<A, any>, args: SchemaInput<A>, opts?: ToolSubscribeOptions): Promise<Subscription>;
  /**
   * Ends the calling conversation's subscription to event with args,
   * resolving to whether one ended. For a native caller it ends the user's
   * own (webhook) subscriptions to the event with those arguments.
   */
  unsubscribe<A extends AnySchema>(event: WakeEvent<A, any>, args: SchemaInput<A>): Promise<boolean>;
  /** Finds or starts the calling conversation's session without subscribing it (it doesn't make it wakeable). */
  resolveSession(): Promise<string>;
}

/** A ToolCall used after its handler returned. */
export class CallEnded extends Error {
  constructor() {
    super("wakeshift: the tool call has ended: subscribe before the handler returns");
    this.name = "CallEnded";
  }
}

/** attach's event options. */
export interface EventsOptions {
  /**
   * Asked first whether a wrapped tool's call comes from a client that
   * subscribes to the app's events itself (call.native), for callers the
   * SDK can't tell (an app's own house agents). False leaves it to the
   * SDK's own checks.
   */
  native?: (extra: ToolCallExtra) => boolean;
}

// The line a text-carried beacon is on (the core's beacon.Text), which a
// harness's hook reads only as the last line of a result's text.
const BEACON_LINE_PREFIX = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: ";
const CONNECTOR_META_KEY = "wakeshift/connector";
// The most native lines Wakeshift takes for one call.
const MAX_NATIVE_LINES = 20;

/** /calls/addition's answer. */
export interface CallAddition {
  text?: string;
  trailer?: string;
  meta?: Record<string, unknown>;
}

type Content = CallToolResult["content"][number];

// takeTrailer takes a beacon line off the end of the result's text
// (dropping its block if nothing else is in it), returning it ("" if
// there's none).
function takeTrailer(content: Content[]): string {
  const last = content.at(-1);
  if (!last || last.type !== "text") return "";
  const text = last.text.replace(/[ \t\r\n]+$/, "");
  const i = text.lastIndexOf("\n");
  const line = i >= 0 ? text.slice(i + 1) : text;
  if (!line.startsWith(BEACON_LINE_PREFIX) || !line.endsWith(")")) return "";
  const rest = i >= 0 ? text.slice(0, i).replace(/[ \t\r\n]+$/, "") : "";
  if (rest === "") content.pop();
  else content[content.length - 1] = { ...last, text: rest };
  return line;
}

/**
 * @internal Adds the call's block to a wrapped handler's result: Wakeshift's
 * text as a block after the app's content, then a text-carried beacon last,
 * and _meta; a string result stays one text block. A result that already
 * carries a beacon keeps it, last, and gets no second one. With wakeOutput,
 * structuredContent's wakeshift is the block's text. test/wrapped-block.json
 * is the shape, shared with the other SDKs.
 */
export function mergeBlock(result: WrappedResult, add: CallAddition, wakeOutput: boolean): CallToolResult {
  const isString = typeof result === "string";
  const base: CallToolResult = isString ? { content: [{ type: "text", text: result }] } : result;
  const out: CallToolResult = { ...base, content: [...(base.content ?? [])] };
  const text = (add.text ?? "").trim();
  const metaBeacon = !!out._meta && CONNECTOR_META_KEY in out._meta;
  const existing = takeTrailer(out.content);
  if (text) out.content.push({ type: "text", text });
  const has = metaBeacon || existing !== "";
  if (existing) out.content.push({ type: "text", text: existing });
  else if (!has && add.trailer) out.content.push({ type: "text", text: add.trailer });
  for (const [k, v] of Object.entries(add.meta ?? {})) {
    if (k === CONNECTOR_META_KEY && has) continue;
    out._meta = { ...(out._meta ?? {}), [k]: v };
  }
  if (isString) {
    const texts = out.content.map((c) => (c.type === "text" ? c.text : "")).filter((t) => t !== "");
    out.content = [{ type: "text", text: texts.join("\n\n") }];
  }
  if (text && wakeOutput && out.structuredContent && typeof out.structuredContent === "object" && !Array.isArray(out.structuredContent)) {
    out.structuredContent = { ...out.structuredContent, wakeshift: text };
  }
  return out;
}

// metaString is the string at key in a tool call's _meta: the key itself,
// else a dotted path into nested objects (the core's harnesses.metaString).
function metaString(meta: Record<string, unknown> | undefined, key: string): string {
  if (!meta) return "";
  const limit = (s: unknown) => (typeof s === "string" && s.length <= 200 ? s : "");
  if (typeof meta[key] === "string") return limit(meta[key]);
  let cur: unknown = meta;
  for (const part of key.split(".")) {
    if (!cur || typeof cur !== "object" || Array.isArray(cur)) return "";
    cur = (cur as Record<string, unknown>)[part];
  }
  return limit(cur);
}

/** @internal Whether a tool call's _meta names its conversation, the way a harness puts it there (test/conversation-meta.json). */
export function namesConversation(meta: Record<string, unknown> | undefined): boolean {
  return CONVERSATION_META.some((k) => metaString(meta, k) !== "");
}

function sameJSON(a: unknown, b: unknown): boolean {
  const norm = (v: unknown): unknown =>
    v === null || v === undefined
      ? {}
      : Array.isArray(v)
        ? v.map(norm)
        : typeof v === "object"
          ? Object.fromEntries(Object.keys(v as object).sort().map((k) => [k, norm((v as Record<string, unknown>)[k])]))
          : v;
  return JSON.stringify(norm(a)) === JSON.stringify(norm(b));
}

function detailOf(err: unknown): string {
  return err instanceof WakeshiftError ? err.detail : String((err as Error)?.message ?? err);
}

// The parts of an McpServer the wrapper reads that aren't public: its
// registered tools (the handler, input and output schemas). A unit test pins
// them against the supported MCP SDK versions.
interface RegisteredToolInternal {
  inputSchema?: AnySchema;
  outputSchema?: AnySchema;
  handler?: unknown;
  callback?: unknown;
}

function registeredTool(server: McpServer, name: string): RegisteredToolInternal | undefined | null {
  const tools = (server as unknown as { _registeredTools?: Record<string, RegisteredToolInternal> })._registeredTools;
  if (!tools || typeof tools !== "object") return null; // an MCP SDK that keeps them elsewhere
  return tools[name];
}

interface CreatedSub {
  name: string;
  arguments: unknown;
  session: string;
  pending: string; // the call's pending-addition record, which made it
}

interface NativeLine {
  name: string;
  arguments: unknown;
}

/** @internal What the wrapper needs of attach: the client, the server, who's calling, the event options. */
export interface AttachContext {
  wake: Wakeshift;
  server: McpServer;
  /** Who attach's identify says is calling: its user and client. */
  identify(extra: ToolCallExtra): Identity | undefined;
  /** The client's name: the request's (initialize's, else _meta's), else identified (identify's), else "". */
  clientOf(meta: Record<string, unknown> | undefined, identified?: string): string;
  events: EventsOptions;
  warnOnce(key: string, message: string): void;
}

/** One tools/call of a wrapped tool: what its handler subscribed, for the block and the rollback. */
class CallScope implements ToolCall {
  user: string | undefined;
  tool: string;
  native = false;
  nativeConversation: string | null = null;
  session: string | null;
  recognized: Recognized | null = null;
  extra: ToolCallExtra;

  ended = false;
  pending = "";
  armed = false;
  created: CreatedSub[] = [];
  nativeLines: NativeLine[] = [];
  inflight = new Set<Promise<unknown>>();
  private queue: Promise<unknown> = Promise.resolve();
  // The client's name identify gave, for a request that names none.
  private readonly identified: string | undefined;

  constructor(
    private readonly at: AttachContext,
    tool: string,
    extra: ToolCallExtra,
    private readonly args: { session?: string; harness?: string; conversation?: string },
  ) {
    this.tool = tool;
    this.extra = extra;
    const id = at.identify(extra);
    this.user = id?.user;
    this.identified = id?.client;
    this.session = args.session || null;
    const n = nativeOf(at, extra, this.identified);
    this.native = n.native;
    this.nativeConversation = n.conversation;
  }

  // wire is the call as Wakeshift takes it: the request's client, _meta and
  // User-Agent, the agent's harness and conversation_id arguments, the
  // conversation's session, and who's calling.
  wire(session = this.session): Call {
    const meta = this.extra._meta as Record<string, unknown> | undefined;
    return packCall({
      meta,
      clientName: this.at.clientOf(meta, this.identified),
      user: this.user,
      session: session ?? undefined,
      userAgent: userAgentOf(this.extra),
      harness: this.args.harness,
      conversation: this.args.conversation,
      tool: this.tool,
    });
  }

  // request runs one request of the call to Wakeshift, after any before it
  // (one at a time per call), refusing once the handler has returned.
  private request<T>(fn: () => Promise<T>): Promise<T> {
    if (this.ended) return Promise.reject(new CallEnded());
    const run = this.queue.then(() => (this.ended ? Promise.reject(new CallEnded()) : fn()));
    this.queue = run.catch(() => undefined);
    this.inflight.add(run);
    run.then(
      () => this.inflight.delete(run),
      () => this.inflight.delete(run),
    );
    return run;
  }

  private checkEvent(event: WakeEvent<any, any>): void {
    if (!event || typeof event.owner !== "function" || event.owner() !== this.at.wake) {
      throw new WakeshiftError(404, "unknown_event", `${event?.name ?? "that"} is another app's event: this app doesn't offer it`);
    }
  }

  subscribe<A extends AnySchema>(event: WakeEvent<A, any>, args: SchemaInput<A>, opts: ToolSubscribeOptions = {}): Promise<Subscription> {
    return this.request(async () => {
      this.checkEvent(event);
      const known = event.parseArgs(args);
      if (this.native) {
        this.addNative(event.name, known);
        return { id: "", user: this.user ?? "", created: false, session: "", recognized: { harness: null, via: null, claimed: null }, native: true };
      }
      if (event.personal && !this.user) {
        throw new WakeshiftError(400, "bad_arguments", `${event.name} is personal: call.subscribe needs a user (attach's identify), who the subscription belongs to`);
      }
      const body: Record<string, unknown> = { call: this.wire(), name: event.name, arguments: known, defer_addition: true };
      if (this.pending) body.pending = this.pending;
      if (opts.ttl !== undefined) body.ttl = opts.ttl;
      if (opts.reason) body.reason = opts.reason;
      if (opts.label) body.label = opts.label;
      const out = await this.at.wake.requestApp<{
        event_subscription: { id: string; user?: string; created: boolean };
        session: string;
        agent?: Partial<Recognized>;
        pending?: string;
      }>("POST", "/event-subscriptions", body);
      if (out.pending) this.pending = out.pending;
      this.armed = true;
      this.session = out.session;
      const rec = recognizedOf(out.agent);
      this.recognized = rec;
      const es = out.event_subscription;
      if (es.created) this.created.push({ name: event.name, arguments: known, session: out.session, pending: out.pending ?? this.pending });
      return { id: es.id, user: es.user ?? this.user ?? "", created: !!es.created, session: out.session, recognized: rec, native: false };
    });
  }

  unsubscribe<A extends AnySchema>(event: WakeEvent<A, any>, args: SchemaInput<A>): Promise<boolean> {
    return this.request(async () => {
      this.checkEvent(event);
      const known = event.parseArgs(args);
      if (this.native) return this.endNative(event.name, known);
      const out = await this.at.wake.requestApp<{ ended?: boolean }>("POST", "/event-subscriptions/unsubscribe", { call: this.wire(), name: event.name, arguments: known });
      return !!out.ended;
    });
  }

  resolveSession(): Promise<string> {
    return this.request(async () => {
      if (this.pending && this.session) return this.session;
      const out = await this.at.wake.requestApp<{ session: string; pending: string; agent?: Partial<Recognized> }>("POST", "/calls/session", { call: this.wire() });
      this.pending = out.pending;
      this.session = out.session;
      this.recognized = recognizedOf(out.agent);
      return out.session;
    });
  }

  // endNative ends the user's webhook subscriptions to event with these arguments.
  private async endNative(name: string, args: unknown): Promise<boolean> {
    if (!this.user) return false;
    const out = await this.at.wake.requestApp<{ subscriptions?: { id: string; event: string; arguments: unknown; destination: string }[] }>("POST", "/event-subscriptions/list", {
      user: this.user,
    });
    let ended = false;
    for (const s of out.subscriptions ?? []) {
      if (s.event !== name || s.destination !== "webhook" || !sameJSON(s.arguments, args)) continue;
      const r = await this.at.wake.requestApp<{ ended?: boolean }>("POST", "/event-subscriptions/end", { user: this.user, id: s.id });
      ended = ended || !!r.ended;
    }
    return ended;
  }

  // addNative adds a native line, once per event and arguments.
  private addNative(name: string, args: unknown): void {
    if (this.nativeLines.some((l) => l.name === name && sameJSON(l.arguments, args))) return;
    if (this.nativeLines.length === MAX_NATIVE_LINES) {
      this.at.warnOnce(
        "native:" + this.tool,
        `wakeshift: the wrapped tool ${this.tool} subscribed a native client to more than ${MAX_NATIVE_LINES} events in one call; only ${MAX_NATIVE_LINES} are named`,
      );
      return;
    }
    this.nativeLines.push({ name, arguments: args });
  }

  // end closes the call to further requests, after waiting for any in
  // flight (warning once per tool: the handler should have waited for them).
  async end(): Promise<void> {
    if (this.inflight.size > 0) {
      this.at.warnOnce(
        "inflight:" + this.tool,
        `wakeshift: the wrapped tool ${this.tool} returned while a subscribe was in flight; it was waited for: await call.subscribe, call.unsubscribe and call.resolveSession before returning`,
      );
      await Promise.allSettled([...this.inflight]);
    }
    this.ended = true;
  }

  // rollback ends every subscription the call created and no other call
  // has renewed since (the core checks the pending record that made it),
  // with reason rolled_back (audited as such, no "ended" notice), whatever
  // became of the request.
  async rollback(): Promise<void> {
    for (const c of this.created) {
      try {
        await this.at.wake.requestApp("POST", "/event-subscriptions/unsubscribe", { call: this.wire(c.session), name: c.name, arguments: c.arguments, reason: "rolled_back", pending: c.pending });
      } catch (err) {
        this.at.wake.warn(`wakeshift: rolling back a failed tool call's subscription (${this.tool}, ${c.name}): ${detailOf(err)}`);
      }
    }
    this.created = [];
  }

  // addition asks Wakeshift for the call's block: what the conversation
  // needs to be woken (from the call's pending-addition record, if a
  // subscribe or resolveSession made one; otherwise what any other tool
  // result of the app's carries), and a line per event a native caller
  // should subscribe to. A call that names no conversation, and subscribed
  // nothing, gets nothing (null). Only a failure for a call that subscribed
  // is thrown: without a subscription, the block is a courtesy.
  async addition(): Promise<CallAddition | null> {
    const wire = this.wire();
    if (!this.pending && this.nativeLines.length === 0 && !wire.session && !wire.conversation && !namesConversation(wire.meta)) return null;
    const body: Record<string, unknown> = { call: wire };
    if (this.pending) body.pending = this.pending;
    if (this.nativeLines.length > 0) body.native = this.nativeLines;
    try {
      return await this.at.wake.requestApp<CallAddition>("POST", "/calls/addition", body);
    } catch (err) {
      if (this.armed || this.nativeLines.length > 0) throw err;
      this.at.wake.warn(`wakeshift: Wakeshift's addition to a result of ${this.tool}: ${detailOf(err)}`);
      return null;
    }
  }
}

function recognizedOf(a: Partial<Recognized> | undefined): Recognized {
  return { harness: a?.harness ?? null, via: a?.via ?? null, claimed: a?.claimed ?? null };
}

function headerOf(extra: ToolCallExtra, name: string): string {
  const h = extra?.requestInfo?.headers as Record<string, string | string[] | undefined> | undefined;
  if (!h) return "";
  const v = h[name] ?? h[name.toLowerCase()];
  return (Array.isArray(v) ? v[0] : v) ?? "";
}

function userAgentOf(extra: ToolCallExtra): string {
  return headerOf(extra, "user-agent") || headerOf(extra, "User-Agent");
}

function hasKey(o: unknown, key: string): boolean {
  return !!o && typeof o === "object" && !Array.isArray(o) && (o as Record<string, unknown>)[key] !== undefined;
}

/**
 * @internal Whether a call comes from a client that subscribes to the app's
 * events itself, on stateless servers too (which never see initialize), in
 * this order: the app's override (events.native); ChatGPT's
 * _meta["openai/session"]; a client name (initialize's, or _meta's, else
 * identify's) or User-Agent of a known native client; a client that
 * declares the MCP Events extension (initialize's capabilities, or
 * _meta's). conversation is ChatGPT's conversation, when it names one.
 */
export function nativeOf(
  at: Pick<AttachContext, "server" | "clientOf" | "events">,
  extra: ToolCallExtra,
  identified?: string,
): { native: boolean; conversation: string | null } {
  const meta = (extra._meta ?? {}) as Record<string, unknown>;
  const conversation = typeof meta["openai/session"] === "string" && meta["openai/session"] ? (meta["openai/session"] as string) : null;
  if (at.events.native?.(extra)) return { native: true, conversation };
  if (conversation) return { native: true, conversation };
  if (dialectOf(at.clientOf(meta, identified), userAgentOf(extra)) === "chatgpt") return { native: true, conversation: null };
  const caps = at.server.server.getClientCapabilities() as Record<string, unknown> | undefined;
  if (hasKey(caps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  const metaCaps = meta["io.modelcontextprotocol/clientCapabilities"] as Record<string, unknown> | undefined;
  if (hasKey(metaCaps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  return { native: false, conversation: null };
}

/**
 * Wakeshift attached to an MCP server: the handle `wake.attach` returns.
 * Wrap a tool's callback with events, and give its input schema
 * Wakeshift's three arguments with inputs.
 */
export class Attached {
  /** @internal */
  private readonly wrapped = new Map<string, unknown>();

  /** @internal Use wake.attach. */
  constructor(private readonly at: AttachContext) {}

  /**
   * shape plus Wakeshift's three optional arguments (session_key, harness
   * and conversation_id), for a wrapped tool's inputSchema:
   * `inputSchema: attached.inputs({ table: z.string() })`.
   */
  inputs<S extends ZodRawShapeCompat>(shape: S): S & WakeInputs {
    for (const k of WAKE_KEYS) if (k in (shape ?? {})) throw new Error(`wakeshift: attached.inputs adds ${k} itself: leave it out of the shape`);
    return { ...shape, ...fieldsFor(shape, "inputs") };
  }

  /**
   * shape plus an optional wakeshift string, for a wrapped tool's
   * outputSchema: the wrapper sets it to the block's text (without the
   * beacon line), for hosts that show the model only structuredContent.
   * The block stays in the content too.
   */
  output<S extends ZodRawShapeCompat>(shape: S): S & WakeOutputShape {
    if ("wakeshift" in (shape ?? {})) throw new Error("wakeshift: attached.output adds wakeshift itself: leave it out of the shape");
    return { ...shape, ...fieldsFor(shape, "output") };
  }

  /**
   * Wraps a tool's callback, for `server.registerTool(name, …)` with the
   * same name (MCP doesn't tell a callback its tool's name): the handler
   * gets the tool's own arguments and a ToolCall, and returns a string or
   * a CallToolResult; the reply gets Wakeshift's block. The tool's
   * inputSchema must come from attached.inputs (a type error otherwise).
   * Register it on the server passed to attach.
   */
  events<S extends WakeShape>(name: string, handler: (args: AppArgs<S>, call: ToolCall) => WrappedResult | Promise<WrappedResult>): ToolCallback<S> {
    if (!name || typeof name !== "string") throw new Error("wakeshift: attached.events needs the tool's name, as registered");
    if (typeof handler !== "function") throw new Error("wakeshift: attached.events needs a handler");
    const cb = (async (args: Record<string, unknown>, extra: ToolCallExtra) => this.run(name, cb, handler as any, args ?? {}, extra)) as unknown as ToolCallback<S>;
    this.wrapped.set(name, cb);
    return cb;
  }

  private async run(
    name: string,
    cb: unknown,
    handler: (args: Record<string, unknown>, call: ToolCall) => WrappedResult | Promise<WrappedResult>,
    input: Record<string, unknown>,
    extra: ToolCallExtra,
  ): Promise<CallToolResult> {
    const tool = registeredTool(this.at.server, name);
    if (tool === undefined || (tool && (tool.handler ?? tool.callback) !== cb)) {
      throw new Error(`wakeshift: attached.events("${name}", …) isn't the tool named ${name} on the server passed to attach: register it with the same name, on that server`);
    }
    let wakeOutput = false;
    let outputSchema: AnySchema | undefined;
    if (tool) {
      const shape = getObjectShape(normalizeObjectSchema(tool.inputSchema));
      if (!shape || WAKE_KEYS.some((k) => !(k in shape))) {
        throw new Error(`wakeshift: the tool ${name} doesn't take Wakeshift's arguments: make its inputSchema with attached.inputs(…)`);
      }
      outputSchema = tool.outputSchema;
      const out = getObjectShape(normalizeObjectSchema(outputSchema));
      wakeOutput = !!out && "wakeshift" in out;
    } else {
      this.at.warnOnce("internals", "wakeshift: this MCP SDK keeps its tools where Wakeshift can't read them, so wrapped tools' registration and output aren't checked");
    }
    const str = (k: string) => (typeof input[k] === "string" && (input[k] as string) ? (input[k] as string) : undefined);
    const call = new CallScope(this.at, name, extra, { session: str(SESSION_ARG), harness: str(HARNESS_ARG), conversation: str(CONVERSATION_ARG) });
    const appArgs: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(input)) if (!(WAKE_KEYS as readonly string[]).includes(k)) appArgs[k] = v;

    let result: WrappedResult;
    try {
      result = await handler(appArgs, call);
    } catch (err) {
      await call.end();
      await call.rollback();
      throw err;
    }
    await call.end();
    // Rollback is decided from the final result, the one the client would
    // receive: MCP validates the output after this callback returns, so
    // the wrapper checks it the same way first.
    if (result === null || result === undefined || (typeof result !== "string" && typeof result !== "object")) {
      await call.rollback();
      return result as unknown as CallToolResult;
    }
    if ((typeof result === "object" && result.isError) || extra?.signal?.aborted || !(await validOutput(outputSchema, result))) {
      await call.rollback();
      return typeof result === "string" ? { content: [{ type: "text", text: result }] } : result;
    }
    let add: CallAddition | null;
    try {
      add = await call.addition();
    } catch (err) {
      // The conversation is subscribed but wouldn't learn how it's woken:
      // the call fails, as one whose handler failed does.
      await call.rollback();
      this.at.wake.warn(`wakeshift: Wakeshift's block for a result of ${name} failed, so the call's subscriptions were rolled back: ${detailOf(err)}`);
      return { isError: true, content: [{ type: "text", text: `Wakeshift couldn't finish setting up this conversation's wakes (${detailOf(err)}). Try again.` }] };
    }
    const out = mergeBlock(result, add ?? {}, wakeOutput);
    if ((add?.text ?? "").trim() && !wakeOutput && out.structuredContent) {
      this.at.warnOnce(
        "output:" + name,
        `wakeshift: the wrapped tool ${name} returns structuredContent without attached.output: hosts that show only structuredContent won't see how this conversation is woken`,
      );
    }
    return out;
  }
}

// validOutput mirrors McpServer's own check of a result against the tool's
// outputSchema (validateToolOutput): one it would refuse fails the call.
async function validOutput(outputSchema: AnySchema | undefined, result: WrappedResult): Promise<boolean> {
  if (!outputSchema) return true;
  if (typeof result === "string" || !result.structuredContent) return false;
  const obj = normalizeObjectSchema(outputSchema);
  if (!obj) return true;
  return (await safeParseAsync(obj, result.structuredContent)).success;
}
