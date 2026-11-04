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
//     attached.events("join", { subscribes: [turn] }, async ({ table }, call) => {
//       const sub = await call.subscribe(turn, { table }, { reason: `Playing at ${table}` })
//       await casino.seat(call.user!, table, sub.id)
//       return `You're seated at ${table}.`
//     }))

import { UNNAMED_APP, unnamedNotifiesParts, withSubscribes } from "./events.js";
import { z } from "zod";
import * as z3 from "zod/v3";
import * as z4 from "zod/v4";
import * as z4mini from "zod/v4-mini";
import type { CallToolResult, ToolCallExtra } from "./mcp.js";
import { getObjectShape, isSchema, isZ4Schema, normalizeObjectSchema, type AnySchema, type SchemaInput, type ShapeOutput, type ZodRawShapeCompat } from "./zod.js";
import type { Host } from "./host.js";
import { WakeshiftError } from "./errors.js";
import { EXTENSION_KEY, dialectOf } from "./spec.js";
import { takesTasks } from "./discover.js";
import type { Identity, WakeEvent } from "./events.js";
import { packCall, userAgentOf, type Call, type Duration, type Recognized, type Wakeshift } from "./wakeshift.js";

// Wakeshift's three arguments (wakeshift.ts's SESSION_ARG, HARNESS_ARG and
// CONVERSATION_ARG, repeated: that module imports this one).
const SESSION_ARG = "session_key";
const HARNESS_ARG = "harness";
const CONVERSATION_ARG = "conversation_id";

export type { ToolCallExtra };

// What the agent is told each of Wakeshift's arguments is: the Go SDK's
// WakeArgs, word for word.
const SESSION_KEY_DESCRIPTION =
  "This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it.";
const HARNESS_DESCRIPTION =
  "The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed";
const CONVERSATION_DESCRIPTION =
  "Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty";
const WAKESHIFT_OUTPUT_DESCRIPTION = "What Wakeshift adds for this conversation: how it's woken by this app's events";
/** @internal Wakeshift's three arguments' descriptions, for a tool whose input schema is written out (the wait tool, tasks.ts). */
export const WAKE_ARG_DESCRIPTIONS = { session_key: SESSION_KEY_DESCRIPTION, harness: HARNESS_DESCRIPTION, conversation_id: CONVERSATION_DESCRIPTION } as const;

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
// (for an empty shape, the default import's, or zod 4's for the MCP SDK
// 2.x, which takes only zod 4).
function fieldsFor<K extends "inputs" | "output">(shape: ZodRawShapeCompat, kind: K, host?: Host): K extends "inputs" ? WakeInputs : WakeOutputShape {
  const first = Object.values(shape ?? {})[0];
  const version = first ? (isZ4Schema(first) ? "v4" : "v3") : host?.version === 2 ? "v4" : null;
  const fields = version ? byVersion[version][kind] : kind === "inputs" ? wakeInputs : wakeOutput;
  return fields as any;
}

/** @internal Wakeshift's three arguments as attach's own tools take them, in the zod the host's MCP SDK takes. */
export function wakeInputsFor(host: Host): ZodRawShapeCompat {
  return host.version === 2 ? byVersion.v4.inputs : wakeInputs;
}

// withFields is an object schema with fields added, in the schema's own zod
// API (zod 3, zod 4 classic or mini).
function withFields(schema: AnySchema, fields: ZodRawShapeCompat): AnySchema {
  const s = schema as { extend?: (f: ZodRawShapeCompat) => AnySchema };
  if (typeof s.extend === "function") return s.extend(fields);
  return z4mini.extend(schema as any, fields as any) as AnySchema;
}

/** Wakeshift's three arguments, as attached.inputs adds them. */
export type WakeInputs = typeof wakeInputs;
/** The optional wakeshift string attached.output adds. */
export type WakeOutputShape = typeof wakeOutput;
/** Wakeshift's three arguments in zod 4, as attached.inputs adds them to a zod 4 object. */
export type WakeInputsV4 = typeof byVersion.v4.inputs;
/** Wakeshift's three arguments in zod 3, as attached.inputs adds them to a zod 3 object. */
export type WakeInputsV3 = typeof byVersion.v3.inputs;
/** The optional wakeshift string in zod 4, as attached.output adds it to a zod 4 object. */
export type WakeOutputV4 = typeof byVersion.v4.output;
/** The optional wakeshift string in zod 3, as attached.output adds it to a zod 3 object. */
export type WakeOutputV3 = typeof byVersion.v3.output;
/** What a wrapped tool's arguments must include: Wakeshift's three (attached.inputs adds them). */
export type WakeArgs = { [K in WakeKey]?: string | undefined };
/**
 * The callback attached.events returns, for `server.registerTool`: it takes
 * the tool's arguments (A, Wakeshift's three included) and your MCP SDK's
 * request context (C).
 */
export type WrappedCallback<A, C> = (args: A, extra: C) => Promise<any>;
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

/** Any of the app's events: what call.subscribe takes in a tool that doesn't declare its events. */
export type AnyWakeEvent = WakeEvent<any, any, any>;

/** The arguments event takes, as call.subscribe takes them. */
export type EventArgs<E> = E extends WakeEvent<infer A, any, any> ? SchemaInput<A> : never;

/**
 * What every ToolCall has, whatever its tool subscribes to: who's calling,
 * unsubscribe and resolveSession. Only the wrapper makes one, and it works
 * until the handler returns (then CallEnded).
 */
export interface ToolCallBase<C = any> {
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
  /** The call's request context, as your MCP SDK passes it to the tool's callback: 2.x's ctx, 1.x's extra. */
  readonly extra: C;
  /**
   * Ends the calling conversation's subscription to event with args,
   * resolving to whether one ended. For a native caller it ends the user's
   * own (webhook) subscriptions to the event with those arguments.
   */
  unsubscribe<A extends AnySchema>(event: WakeEvent<A, any>, args: SchemaInput<A>): Promise<boolean>;
  /** Finds or starts the calling conversation's session without subscribing it (it doesn't make it wakeable). */
  resolveSession(): Promise<string>;
}

/** call.subscribe, for a tool that subscribes to the events E. */
export interface ToolCallSubscribe<E extends AnyWakeEvent = AnyWakeEvent> {
  /**
   * Subscribes the calling conversation to event with args, or renews its
   * subscription. The reply then carries what the conversation needs to be
   * woken (one block for the call, however many subscribes it makes), and
   * if the call fails, the subscription is ended. For a native caller
   * nothing is made. Your tool decides who may: authorize isn't consulted.
   * A personal event needs a user (identify's). event is one of the events
   * the tool declares (attached.events' subscribes).
   */
  subscribe<Ev extends E>(event: Ev, args: EventArgs<Ev>, opts?: ToolSubscribeOptions): Promise<Subscription>;
}

/**
 * One call of a wrapped tool (attached.events): who's calling, and the
 * calls that subscribe the calling conversation. E is the events the tool
 * declares (`attached.events(name, { subscribes: [...] }, handler)`):
 * call.subscribe takes only those, and a tool that declares none has no
 * call.subscribe. A tool wrapped without subscribes gets a call whose
 * subscribe takes any of the app's events, and warns when it's used.
 */
export type ToolCall<C = any, E extends AnyWakeEvent = AnyWakeEvent> = ToolCallBase<C> & ([E] extends [never] ? {} : ToolCallSubscribe<E>);

/** attached.events' options for a tool. */
export interface ToolEventsOptions<E extends readonly AnyWakeEvent[] = readonly AnyWakeEvent[]> {
  /**
   * The events the tool subscribes the calling conversation to (with
   * call.subscribe). tools/list then adds a sentence to the tool's
   * description saying the tool is how the app notifies the agent of them,
   * in their own descriptions' words, and that the result may ask to start
   * a listener and offer an install. Agents set Wakeshift's part of a
   * result aside as unrequested unless the tool's description says so. An
   * empty list declares that the tool subscribes to nothing.
   */
  subscribes: E;
}

/**
 * @internal What a subscribe the tool doesn't declare does: warns once per
 * tool (this release) or throws (error: the next release). Tests flip it.
 */
export const undeclaredSubscribe = { error: false };

// eventRef is how a warning writes an event in its fix: the name of the
// event wake.defineEvent returned.
const eventRef = (name: string) => `<${name}'s event>`;

/** @internal The warning (or, with undeclaredSubscribe.error, the error) for a subscribe the tool doesn't declare. */
export function undeclaredMessage(tool: string, event: string, declared: readonly string[] | null, error: boolean): string {
  const all = [...(declared ?? []), event];
  const fix = `attached.events(${JSON.stringify(tool)}, { subscribes: [${all.map(eventRef).join(", ")}] }, handler)`;
  const what =
    declared === null
      ? `the tool ${tool} subscribed to ${event} without declaring the events it subscribes to`
      : declared.length === 0
        ? `the tool ${tool} subscribed to ${event}, but declares that it subscribes to nothing`
        : `the tool ${tool} subscribed to ${event}, which its subscribes doesn't list (${declared.join(", ")})`;
  if (error) return `wakeshift: ${what}, so the subscribe was refused. Fix: ${fix}.`;
  return `wakeshift: ${what}, so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: ${fix}. It works for now; from the next release an undeclared subscribe throws.`;
}

/** A ToolCall used after its handler returned. */
export class CallEnded extends Error {
  constructor() {
    super("wakeshift: the tool call has ended: subscribe before the handler returns");
    this.name = "CallEnded";
  }
}

/** attach's event options. */
export interface EventsOptions<C = any> {
  /**
   * Asked first whether a wrapped tool's call comes from a client that
   * subscribes to the app's events itself (call.native), for callers the
   * SDK can't tell (an app's own house agents). False leaves it to the
   * SDK's own checks. It gets the call's request context, as identify does.
   */
  native?(extra: C): boolean;
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

// dropTrailers takes every beacon line off the end of the result's last
// text (dropping its block if nothing else is in it). Only Wakeshift's own
// trailer may end a wrapped tool's text: a hook acts on the beacon there,
// and one in the app's text may be someone else's, relayed (a chat message
// carrying a beacon minted for another session).
function dropTrailers(content: Content[]): void {
  for (;;) {
    let i = content.length - 1;
    while (i >= 0 && content[i]!.type !== "text") i--;
    if (i < 0) return;
    const block = content[i] as Extract<Content, { type: "text" }>;
    const text = block.text.replace(/[ \t\r\n]+$/, "");
    const j = text.lastIndexOf("\n");
    const line = j >= 0 ? text.slice(j + 1) : text;
    if (!line.startsWith(BEACON_LINE_PREFIX) || !line.endsWith(")")) return;
    const rest = j >= 0 ? text.slice(0, j).replace(/[ \t\r\n]+$/, "") : "";
    if (rest === "") content.splice(i, 1);
    else content[i] = { ...block, text: rest };
  }
}

/** The structuredContent field a wrapped tool's structured result carries Wakeshift's block in (the same in every SDK). */
export const WAKESHIFT_FIELD = "wakeshift";

/**
 * @internal An object output schema (JSON Schema, as tools/list gives it)
 * with an optional wakeshift string added, as a copy, so a client that
 * checks structuredContent against it takes the block. Anything else, or a
 * schema that already names it, comes back as it was.
 * test/wrapped-block.json (output_schemas) pins it.
 */
export function withWakeshiftField(schema: unknown): unknown {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) return schema;
  const s = schema as Record<string, unknown>;
  if (s.type !== "object") return schema;
  const props = s.properties;
  if (props !== undefined && (!props || typeof props !== "object" || Array.isArray(props))) return schema;
  if (props && WAKESHIFT_FIELD in (props as object)) return schema;
  return { ...s, properties: { ...((props as object) ?? {}), [WAKESHIFT_FIELD]: { type: "string", description: WAKESHIFT_OUTPUT_DESCRIPTION } } };
}

/**
 * @internal Adds the call's block to a wrapped handler's result: Wakeshift's
 * text as a block after the app's content, then a text-carried beacon last,
 * and _meta; a string result stays one text block. Beacon lines ending the
 * app's own text are dropped (dropTrailers), whether or not there's a block
 * to add. A result whose own _meta carries a connector beacon gets no second
 * one, in _meta or its text. A structured
 * result (structuredContent an object) gets the block's text in its
 * wakeshift field too. test/wrapped-block.json is the shape, shared with
 * the other SDKs.
 */
export function mergeBlock(result: WrappedResult, add: CallAddition): CallToolResult {
  const isString = typeof result === "string";
  const base: CallToolResult = isString ? { content: [{ type: "text", text: result }] } : result;
  const out: CallToolResult = { ...base, content: [...(base.content ?? [])] };
  const text = (add.text ?? "").trim();
  const metaBeacon = !!out._meta && CONNECTOR_META_KEY in out._meta;
  dropTrailers(out.content);
  if (text) out.content.push({ type: "text", text });
  if (!metaBeacon && add.trailer) out.content.push({ type: "text", text: add.trailer });
  for (const [k, v] of Object.entries(add.meta ?? {})) {
    if (k === CONNECTOR_META_KEY && metaBeacon) continue;
    out._meta = { ...(out._meta ?? {}), [k]: v };
  }
  if (isString) {
    const texts = out.content.map((c) => (c.type === "text" ? c.text : "")).filter((t) => t !== "");
    out.content = [{ type: "text", text: texts.join("\n\n") }];
  }
  if (text && out.structuredContent && typeof out.structuredContent === "object" && !Array.isArray(out.structuredContent)) {
    out.structuredContent = { ...(out.structuredContent as Record<string, unknown>), [WAKESHIFT_FIELD]: text };
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
  /** The server, through what attach needs of its MCP SDK (host.ts). */
  host: Host;
  /** Who attach's identify says is calling: its user and client (raw: the MCP SDK's own request context). */
  identify(raw: unknown): Identity | undefined;
  /** The client's name: the request's (initialize's, else _meta's), else identified (identify's), else "". */
  clientOf(meta: Record<string, unknown> | undefined, identified?: string): string;
  events: EventsOptions<any>;
  warnOnce(key: string, message: string): void;
  /** attach serves the wait tool (AttachOptions.wait): only then does a call say its client takes tasks. */
  wait?: boolean;
}

/** @internal One tools/call of a wrapped tool: what its handler subscribed, for the block and the rollback. */
export class CallScope<C> implements ToolCall<C> {
  /** The events the tool declares (null: it declares none: attached.events without subscribes). */
  readonly declared: readonly AnyWakeEvent[] | null;
  user: string | undefined;
  tool: string;
  native = false;
  nativeConversation: string | null = null;
  session: string | null;
  recognized: Recognized | null = null;
  extra: C;

  ended = false;
  pending = "";
  armed = false;
  created: CreatedSub[] = [];
  nativeLines: NativeLine[] = [];
  // Wakeshift's notes from the call's unsubscribes (one that ended nothing
  // because the call passed no session key), for the block.
  notes: string[] = [];
  inflight = new Set<Promise<unknown>>();
  private queue: Promise<unknown> = Promise.resolve();
  // The client's name identify gave, for a request that names none.
  private readonly identified: string | undefined;

  constructor(
    private readonly at: AttachContext,
    tool: string,
    extra: C,
    private readonly args: { session?: string; harness?: string; conversation?: string },
    declared: readonly AnyWakeEvent[] | null = null,
  ) {
    this.declared = declared;
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
    const req = this.at.host.req(this.extra);
    const meta = req.meta;
    return packCall({
      meta,
      clientName: this.at.clientOf(meta, this.identified),
      user: this.user,
      session: session ?? undefined,
      userAgent: userAgentOf(req),
      harness: this.args.harness,
      conversation: this.args.conversation,
      tool: this.tool,
      tasks: !!this.at.wait && takesTasks(meta, req),
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

  // checkDeclared warns once per tool (or throws, once undeclared subscribes
  // are an error) when the tool doesn't declare event.
  private checkDeclared(event: AnyWakeEvent): void {
    const d = this.declared;
    if (d && d.some((e) => e === event || e.name === event.name)) return;
    const names = d ? d.map((e) => e.name) : null;
    if (undeclaredSubscribe.error) throw new Error(undeclaredMessage(this.tool, event.name, names, true));
    this.at.warnOnce("undeclared:" + this.tool, undeclaredMessage(this.tool, event.name, names, false));
  }

  subscribe<Ev extends AnyWakeEvent>(event: Ev, args: EventArgs<Ev>, opts: ToolSubscribeOptions = {}): Promise<Subscription> {
    return this.request(async () => {
      this.checkEvent(event);
      this.checkDeclared(event);
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
      const out = await this.at.wake.requestApp<{ ended?: boolean; addition?: CallAddition }>("POST", "/event-subscriptions/unsubscribe", {
        call: this.wire(),
        name: event.name,
        arguments: known,
      });
      // addition: the call passed no session key, so it ended nothing; the
      // block says to call again with it.
      const note = out.addition?.text?.trim();
      if (note && !this.notes.includes(note)) this.notes.push(note);
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
  //
  // Notes from the call's unsubscribes come first, whatever else it gets.
  async addition(): Promise<CallAddition | null> {
    const add = await this.fetchAddition();
    if (this.notes.length === 0) return add;
    const text = [this.notes.join(" "), add?.text?.trim() ?? ""].filter(Boolean).join("\n\n");
    return { ...(add ?? {}), text };
  }

  private async fetchAddition(): Promise<CallAddition | null> {
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

// isInputRequired is whether a handler's result asks the client for input
// first (resultType input_required, or input requests), as the MCP SDK
// 2.x's isInputRequiredResult and go-sdk's NeedsInput read it.
function isInputRequired(result: WrappedResult): boolean {
  if (!result || typeof result !== "object") return false;
  const r = result as { resultType?: unknown; inputRequests?: unknown };
  return r.resultType === "input_required" || (!!r.inputRequests && typeof r.inputRequests === "object" && Object.keys(r.inputRequests).length > 0);
}

function recognizedOf(a: Partial<Recognized> | undefined): Recognized {
  return { harness: a?.harness ?? null, via: a?.via ?? null, claimed: a?.claimed ?? null };
}

function hasKey(o: unknown, key: string): boolean {
  return !!o && typeof o === "object" && !Array.isArray(o) && (o as Record<string, unknown>)[key] !== undefined;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === "object" && !Array.isArray(v);
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
export function nativeOf(at: Pick<AttachContext, "host" | "clientOf" | "events">, extra: unknown, identified?: string): { native: boolean; conversation: string | null } {
  // A server that doesn't serve MCP Events (an xmcp app's) has no native callers.
  if (!at.host.servesEvents) return { native: false, conversation: null };
  const req = at.host.req(extra);
  const meta = (req.meta ?? {}) as Record<string, unknown>;
  const conversation = typeof meta["openai/session"] === "string" && meta["openai/session"] ? (meta["openai/session"] as string) : null;
  if (at.events.native?.(extra)) return { native: true, conversation };
  if (conversation) return { native: true, conversation };
  if (dialectOf(at.clientOf(meta, identified), userAgentOf(req)) === "chatgpt") return { native: true, conversation: null };
  const caps = at.host.clientCapabilities();
  if (hasKey(caps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  const metaCaps = meta["io.modelcontextprotocol/clientCapabilities"] as Record<string, unknown> | undefined;
  if (hasKey(metaCaps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  return { native: false, conversation: null };
}

/**
 * Wakeshift attached to an MCP server: the handle `wake.attach` returns.
 * Wrap a tool's callback with events, and give its input schema
 * Wakeshift's three arguments with inputs. C is your MCP SDK's request
 * context (2.x's ctx, 1.x's extra), which a wrapped call carries as
 * `call.extra`.
 */
export class Attached<C = any> {
  /** @internal */
  private readonly wrapped = new Map<string, unknown>();
  // The events each wrapped tool declares (attached.events' subscribes), by tool.
  private readonly declared = new Map<string, readonly AnyWakeEvent[]>();

  /** @internal Use wake.attach. */
  constructor(private readonly at: AttachContext) {}

  /**
   * A tool's input schema plus Wakeshift's three optional arguments
   * (session_key, harness and conversation_id), for a wrapped tool's
   * inputSchema: a zod object, `inputSchema: attached.inputs(z.object({ table: z.string() }))`
   * (as @modelcontextprotocol/server 2.x takes it), or a shape,
   * `inputSchema: attached.inputs({ table: z.string() })`.
   */
  inputs<O extends z4.ZodObject>(schema: O): z4.ZodObject<O["shape"] & WakeInputsV4, O["_zod"]["config"]>;
  inputs<O extends z3.AnyZodObject>(schema: O): z3.ZodObject<O["shape"] & WakeInputsV3, O["_def"]["unknownKeys"], O["_def"]["catchall"]>;
  inputs<S extends ZodRawShapeCompat>(shape: S): S & WakeInputs;
  inputs(shape: ZodRawShapeCompat | AnySchema): unknown {
    if (isSchema(shape)) {
      const own = getObjectShape(shape);
      if (!own) throw new Error("wakeshift: attached.inputs takes a zod object or a shape");
      for (const k of WAKE_KEYS) if (k in own) throw new Error(`wakeshift: attached.inputs adds ${k} itself: leave it out of the schema`);
      return withFields(shape, byVersion[isZ4Schema(shape) ? "v4" : "v3"].inputs);
    }
    for (const k of WAKE_KEYS) if (k in (shape ?? {})) throw new Error(`wakeshift: attached.inputs adds ${k} itself: leave it out of the shape`);
    return { ...shape, ...fieldsFor(shape, "inputs", this.at.host) };
  }

  /**
   * A tool's output schema plus an optional wakeshift string, for a wrapped
   * tool's outputSchema (a zod object or a shape, as inputs takes).
   * Optional: every structured result of a wrapped tool gets the block's
   * text (without the beacon line) in wakeshift, for hosts that show the
   * model only structuredContent, and tools/list names it in the tool's
   * output schema; this declares it in your own schema and types. The
   * block stays in the content too.
   */
  output<O extends z4.ZodObject>(schema: O): z4.ZodObject<O["shape"] & WakeOutputV4, O["_zod"]["config"]>;
  output<O extends z3.AnyZodObject>(schema: O): z3.ZodObject<O["shape"] & WakeOutputV3, O["_def"]["unknownKeys"], O["_def"]["catchall"]>;
  output<S extends ZodRawShapeCompat>(shape: S): S & WakeOutputShape;
  output(shape: ZodRawShapeCompat | AnySchema): unknown {
    if (isSchema(shape)) {
      const own = getObjectShape(shape);
      if (!own) throw new Error("wakeshift: attached.output takes a zod object or a shape");
      if (WAKESHIFT_FIELD in own) throw new Error("wakeshift: attached.output adds wakeshift itself: leave it out of the schema");
      return withFields(shape, byVersion[isZ4Schema(shape) ? "v4" : "v3"].output);
    }
    if ("wakeshift" in (shape ?? {})) throw new Error("wakeshift: attached.output adds wakeshift itself: leave it out of the shape");
    return { ...shape, ...fieldsFor(shape, "output", this.at.host) };
  }

  /**
   * Wraps a tool's callback, for `server.registerTool(name, …)` with the
   * same name (MCP doesn't tell a callback its tool's name): the handler
   * gets the tool's own arguments and a ToolCall, and returns a string or
   * a CallToolResult; the reply gets Wakeshift's block. The tool's
   * inputSchema must come from attached.inputs (a type error otherwise).
   * Register it on the server passed to attach.
   *
   * subscribes lists the events the tool subscribes to: call.subscribe
   * takes only those, and tools/list adds a sentence to the tool's
   * description saying it's how the app notifies the agent of them.
   *
   *   attached.events("watch_build", { subscribes: [buildFinished] }, async ({ build }, call) => { … })
   *
   * Without subscribes (attached.events(name, handler)), call.subscribe
   * takes any of the app's events but warns: declare them.
   */
  events<A extends WakeArgs, const E extends readonly AnyWakeEvent[]>(
    name: string,
    opts: ToolEventsOptions<E>,
    handler: (args: Omit<A, WakeKey>, call: ToolCall<C, E[number]>) => WrappedResult | Promise<WrappedResult>,
  ): WrappedCallback<A, C>;
  events<A extends WakeArgs>(name: string, handler: (args: Omit<A, WakeKey>, call: ToolCall<C>) => WrappedResult | Promise<WrappedResult>): WrappedCallback<A, C>;
  events(name: string, optsOrHandler: unknown, maybeHandler?: unknown): WrappedCallback<any, C> {
    if (!name || typeof name !== "string") throw new Error("wakeshift: attached.events needs the tool's name, as registered");
    const declared = typeof optsOrHandler === "function" ? null : this.declaredOf(name, optsOrHandler);
    const handler = typeof optsOrHandler === "function" ? optsOrHandler : maybeHandler;
    if (typeof handler !== "function") throw new Error("wakeshift: attached.events needs a handler");
    const cb = (async (args: Record<string, unknown>, extra: C) => this.run(name, cb, handler as any, args ?? {}, extra)) as unknown as WrappedCallback<any, C>;
    this.wrapped.set(name, cb);
    this.declare(name, declared);
    return cb;
  }

  // declaredOf checks attached.events' options: the tool's subscribes, each
  // one of this app's events.
  private declaredOf(name: string, opts: unknown): readonly AnyWakeEvent[] {
    const list = (opts as { subscribes?: unknown } | null)?.subscribes;
    if (!Array.isArray(list)) throw new Error(`wakeshift: attached.events("${name}", options, handler): options.subscribes lists the events the tool subscribes to ([] for none)`);
    for (const e of list) {
      if (!e || typeof (e as AnyWakeEvent).owner !== "function" || (e as AnyWakeEvent).owner() !== this.at.wake) {
        throw new Error(`wakeshift: attached.events("${name}", …): subscribes lists ${(e as { name?: unknown })?.name ?? String(e)}, which isn't one of this app's events (wake.defineEvent)`);
      }
    }
    return [...list];
  }

  // declare records the events a tool declares (null: none declared).
  private declare(name: string, declared: readonly AnyWakeEvent[] | null): void {
    if (declared) this.declared.set(name, declared);
    else this.declared.delete(name);
  }

  private async run(
    name: string,
    cb: unknown,
    handler: (args: Record<string, unknown>, call: ToolCall<C>) => WrappedResult | Promise<WrappedResult>,
    input: Record<string, unknown>,
    extra: C,
  ): Promise<CallToolResult> {
    const host = this.at.host;
    const tool = host.registeredTool(name);
    if (tool === undefined || (tool && (tool.handler ?? tool.callback) !== cb)) {
      throw new Error(`wakeshift: attached.events("${name}", …) isn't the tool named ${name} on the server passed to attach: register it with the same name, on that server`);
    }
    let outputSchema: unknown;
    if (tool) {
      const shape = getObjectShape(normalizeObjectSchema(tool.inputSchema));
      if (!shape || WAKE_KEYS.some((k) => !(k in shape))) {
        throw new Error(`wakeshift: the tool ${name} doesn't take Wakeshift's arguments: make its inputSchema with attached.inputs(…)`);
      }
      outputSchema = tool.outputSchema;
    } else if (!host.framework) {
      this.at.warnOnce("internals", "wakeshift: this MCP SDK keeps its tools where Wakeshift can't read them, so wrapped tools' registration and output aren't checked");
    }
    const { call, appArgs } = this.begin(name, input, extra);
    let result: WrappedResult;
    try {
      result = await handler(appArgs, call);
    } catch (err) {
      await this.fail(call);
      throw err;
    }
    return this.finish(call, result, outputSchema);
  }

  /**
   * @internal The first half of a wrapped call: the call's scope, from the
   * tool's arguments (Wakeshift's three among them) and the request's
   * context, and the app's own arguments. finish (or fail) ends it.
   */
  begin(name: string, input: Record<string, unknown>, extra: C): { call: CallScope<C>; appArgs: Record<string, unknown> } {
    const str = (k: string) => (typeof input[k] === "string" && (input[k] as string) ? (input[k] as string) : undefined);
    const call = new CallScope<C>(this.at, name, extra, { session: str(SESSION_ARG), harness: str(HARNESS_ARG), conversation: str(CONVERSATION_ARG) }, this.declared.get(name) ?? null);
    const appArgs: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(input ?? {})) if (!(WAKE_KEYS as readonly string[]).includes(k)) appArgs[k] = v;
    return { call, appArgs };
  }

  /** @internal Ends a call whose handler threw: what it subscribed is rolled back. */
  async fail(call: CallScope<C>): Promise<void> {
    await call.end();
    await call.rollback();
  }

  /**
   * @internal The second half of a wrapped call: the handler's result with
   * Wakeshift's block, or rolled back (a failed result, a canceled call, an
   * output its schema refuses). outputSchema is the tool's, as its MCP SDK
   * checks it (undefined: not checked here).
   */
  async finish(call: CallScope<C>, result: WrappedResult, outputSchema: unknown): Promise<CallToolResult> {
    const host = this.at.host;
    const name = call.tool;
    const extra = call.extra;
    await call.end();
    // Rollback is decided from the final result, the one the client would
    // receive: MCP validates the output after this callback returns, so
    // the wrapper checks it the same way first.
    if (result === null || result === undefined || (typeof result !== "string" && typeof result !== "object")) {
      await call.rollback();
      return result as unknown as CallToolResult;
    }
    // A result that asks the client for input (protocol 2026-07-28's
    // input_required) isn't final: the client calls again with the answers,
    // and the handler subscribes again then. Nothing it subscribed stays,
    // and it gets no block (content beside inputRequests is refused).
    if (isInputRequired(result)) {
      await call.rollback();
      return result as CallToolResult;
    }
    if ((typeof result === "object" && result.isError) || host.req(extra).signal?.aborted || !(await host.validOutput(outputSchema, result))) {
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
    const out = mergeBlock(result, add ?? {});
    if (isRecord(out.structuredContent) && WAKESHIFT_FIELD in out.structuredContent && !(await host.validOutput(outputSchema, out))) {
      // A schema that refuses fields it doesn't name (a strict object):
      // MCP would refuse the result, so it goes without the field.
      const { [WAKESHIFT_FIELD]: _dropped, ...own } = out.structuredContent;
      out.structuredContent = own;
      this.at.warnOnce(
        "output:" + name,
        `wakeshift: the wrapped tool ${name}'s outputSchema refuses a wakeshift field, so hosts that show only structuredContent won't see how this conversation is woken: make it with attached.output`,
      );
    }
    return out;
  }

  /**
   * @internal Wraps the server's tools/list handler so each wrapped tool's
   * object output schema names wakeshift (withWakeshiftField). McpServer
   * sets the handler when the first tool is registered, perhaps after
   * attach, so a later set is wrapped too (as tasks.ts does).
   */
  /** @internal Marks name as a wrapped tool, for tools/list (a framework adapter's tool, wrapped outside this handle), with the events it declares (null: none). */
  markWrapped(name: string, declared: readonly AnyWakeEvent[] | null = null): void {
    if (!this.wrapped.has(name)) this.wrapped.set(name, true);
    this.declare(name, declared);
  }

  /** @internal The events a framework adapter's tool declares, checked as attached.events checks them. */
  checkDeclared(name: string, opts: unknown): readonly AnyWakeEvent[] {
    return this.declaredOf(name, opts);
  }

  amendToolsList(): void {
    type Handler = (request: any, extra: any) => Promise<any>;
    const handlers = this.at.host.handlers();
    if (!handlers) return; // a future SDK: schemas stay as registered
    const wrap =
      (h: Handler): Handler =>
      async (request, extra) => {
        const out = await h(request, extra);
        if (out && Array.isArray(out.tools)) {
          this.amendTools(out.tools);
          this.describeTools(out.tools);
          await this.nameApp(out.tools);
        }
        return out;
      };
    const h = handlers.get("tools/list");
    if (h) Map.prototype.set.call(handlers, "tools/list", wrap(h));
    const set = handlers.set.bind(handlers);
    handlers.set = (method: string, h: Handler) => set(method, method === "tools/list" ? wrap(h) : h);
  }

  // describeTools adds the subscribes sentence to each tool that declares
  // events (withSubscribes; "this app" until nameApp names it). A tool's
  // own notifies() sentence stays the only one.
  private describeTools(tools: unknown[]): void {
    for (let i = 0; i < tools.length; i++) {
      const t = tools[i] as { name?: unknown; description?: unknown } | null;
      if (!t || typeof t !== "object" || typeof t.name !== "string") continue;
      const evs = this.declared.get(t.name);
      if (!evs || evs.length === 0) continue;
      const d = typeof t.description === "string" ? t.description : "";
      const described = withSubscribes(d, evs.map((e) => e.description));
      if (described !== d) tools[i] = { ...t, description: described };
    }
  }

  /**
   * notifies(description) with no app named says "this app": tools/list
   * names the app instead, as the console has it (GET /app, asked once). A
   * name Wakeshift doesn't give, or doesn't give in time (wake.appName),
   * leaves "this app".
   */
  private async nameApp(tools: unknown[]): Promise<void> {
    const [first, second] = unnamedNotifiesParts();
    const has = (t: unknown) => {
      const d = (t as { description?: unknown } | null)?.description;
      return typeof d === "string" && d.includes(first) && d.includes(second);
    };
    if (!tools.some(has)) return;
    const name = await this.at.wake.appName();
    if (!name) return;
    const [named1, named2] = [first.replace(UNNAMED_APP, name), second.replace("This app", name.charAt(0).toUpperCase() + name.slice(1))];
    for (let i = 0; i < tools.length; i++) {
      const t = tools[i] as { description: string };
      if (has(t)) tools[i] = { ...t, description: t.description.replace(first, named1).replace(second, named2) };
    }
  }


  private amendTools(tools: unknown[]): void {
    for (let i = 0; i < tools.length; i++) {
      const t = tools[i] as { name?: unknown; outputSchema?: unknown } | null;
      if (!t || typeof t !== "object" || typeof t.name !== "string" || !this.wrapped.has(t.name) || !t.outputSchema) continue;
      const schema = withWakeshiftField(t.outputSchema);
      if (schema !== t.outputSchema) tools[i] = { ...t, outputSchema: schema };
    }
  }
}
