// Tools that subscribe (design §2.4): a vendor tool registered through the
// attachment (attached.registerTool) or named in attach's wrap. Wakeshift
// works at the protocol layer, never on the vendor's callback:
//
//   - tools/call: Wakeshift's three arguments are stripped from every call
//     whose tool doesn't declare them, before the MCP SDK validates and
//     before vendor middleware runs. A call of a tool that subscribes is
//     bound to the async context (wake.currentCall() finds it, or
//     currentCall(ctx) by its request context), and after the MCP SDK's own
//     result validation its result gets Wakeshift's block; a call that
//     fails ends what it subscribed;
//   - tools/list: each tool that subscribes gets Wakeshift's arguments in
//     its JSON inputSchema, the subscribes sentence in its description and
//     the wakeshift field in its outputSchema.
//
// Nothing compares callbacks, so middleware that re-wraps the vendor's
// callback can't break it (#37, CR H3).
//
//   const attached = wake.attach(server, { identify })
//   attached.registerTool("join", { description: "…", inputSchema: z.object({ table: z.string() }), subscribes: [turn] },
//     async ({ table }, ctx, call) => {
//       const sub = await call.subscribe(turn, { table }, { reason: `Playing at ${table}` })
//       return `You're seated at ${table}.`
//     })

import { z } from "zod";
import * as z3 from "zod/v3";
import * as z4 from "zod/v4";
import * as z4mini from "zod/v4-mini";
import type { CallToolResult, HostContext } from "./mcp.js";
import { getObjectShape, isSchema, isZ4Schema, normalizeObjectSchema, type AnySchema, type SchemaInput, type ShapeOutput, type ZodRawShapeCompat } from "./zod.js";
import { stillWrapped, wrapMethod, type Handler, type Host } from "./host.js";
import { UsageError, errorType, fixOf } from "./errors.js";
import { EXTENSION_KEY, bridgedClient, dialectOf } from "./spec.js";
import { takesTasks } from "./discover.js";
import { COMPACT } from "./wording.js";
import { UNNAMED_APP, refusedText, subscriberOf, unnamedNotifiesParts, withSubscribes, type Caller, type Refusal, type WakeEvent } from "./events.js";
import { rememberVouchers } from "./voucherctx.js";
import { logConfigError } from "./log.js";
import { packCall, transportSessionOf, userAgentOf, type Call, type Duration, type Recognized, type Wakeshift } from "./wakeshift.js";

// ── Wakeshift's three arguments ─────────────────────────────────────────

const SESSION_ARG = "session_key";
const HARNESS_ARG = "harness";
const CONVERSATION_ARG = "conversation_id";
/** @internal The names of Wakeshift's three tool arguments. */
export const WAKE_KEYS = [SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG] as const;
/** @internal */
export type WakeKey = (typeof WAKE_KEYS)[number];

// What the agent is told each of Wakeshift's arguments is: the compact
// text (wording.ts, test/wake-args.json compact).
const SESSION_KEY_DESCRIPTION = COMPACT.session_key;
const HARNESS_DESCRIPTION = COMPACT.harness;
const CONVERSATION_DESCRIPTION = COMPACT.conversation_id;
const WAKESHIFT_OUTPUT_DESCRIPTION = "What Wakeshift adds for this conversation";
/** @internal Wakeshift's three arguments' descriptions, for a tool whose input schema is written out (the wait tool, tasks.ts). */
export const WAKE_ARG_DESCRIPTIONS = { session_key: SESSION_KEY_DESCRIPTION, harness: HARNESS_DESCRIPTION, conversation_id: CONVERSATION_DESCRIPTION } as const;

/** @internal Wakeshift's three arguments as JSON Schema properties, as tools/list gives them (test/wake-args.json). */
export const WAKE_ARGS_JSON: Readonly<Record<WakeKey, Readonly<{ type: "string"; description: string }>>> = Object.freeze({
  [SESSION_ARG]: Object.freeze({ type: "string" as const, description: SESSION_KEY_DESCRIPTION }),
  [HARNESS_ARG]: Object.freeze({ type: "string" as const, description: HARNESS_DESCRIPTION }),
  [CONVERSATION_ARG]: Object.freeze({ type: "string" as const, description: CONVERSATION_DESCRIPTION }),
});

function copyArgs(keys: readonly WakeKey[] = WAKE_KEYS): Record<string, { type: string; description: string }> {
  return Object.fromEntries(keys.map((k) => [k, { ...WAKE_ARGS_JSON[k] }]));
}

/**
 * @internal An object input schema (JSON Schema, as tools/list gives it)
 * with Wakeshift's arguments added to its properties (keys: all three by
 * default), as a copy; a property the tool already names is kept. A
 * missing schema is an object with only them; anything that isn't an
 * object schema comes back as it was.
 */
export function withWakeArgsJson(schema: unknown, keys: readonly WakeKey[] = WAKE_KEYS): unknown {
  const copyArgs_ = () => copyArgs(keys);
  if (schema === undefined || schema === null) return { type: "object", properties: copyArgs_() };
  if (typeof schema !== "object" || Array.isArray(schema)) return schema;
  const s = schema as Record<string, unknown>;
  if (s.type !== undefined && s.type !== "object") return schema;
  const props = s.properties;
  if (props !== undefined && (!props || typeof props !== "object" || Array.isArray(props))) return schema;
  return { ...s, type: "object", properties: { ...copyArgs_(), ...((props as object) ?? {}) } };
}

/** The structuredContent field a structured result of a tool that subscribes carries Wakeshift's block in (the same in every SDK). */
export const WAKESHIFT_FIELD = "wakeshift";

// propsOf is a JSON Schema's or a zod schema's top-level property names.
function propsOf(schema: unknown): string[] {
  if (!schema || typeof schema !== "object") return [];
  const json = (schema as { properties?: unknown }).properties;
  if (json && typeof json === "object" && !Array.isArray(json) && !isSchema(schema)) return Object.keys(json);
  const shape = getObjectShape(normalizeObjectSchema(schema));
  return shape ? Object.keys(shape) : [];
}

/** @internal Whether a tool's own input schema names one of Wakeshift's arguments, or its output schema names wakeshift. */
export function namesWakeArgs(input: unknown, output: unknown): boolean {
  const i = propsOf(input);
  return WAKE_KEYS.some((k) => i.includes(k)) || propsOf(output).includes(WAKESHIFT_FIELD);
}

/**
 * @internal An object output schema (JSON Schema, as tools/list gives it)
 * with an optional wakeshift string added, as a copy, so a client that
 * checks structuredContent against it takes the block. Anything else, or a
 * schema that already names it, comes back as it was.
 */
export function withWakeshiftField(schema: unknown): unknown {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) return schema;
  const s = schema as Record<string, unknown>;
  if (s.type !== "object") return schema;
  const props = s.properties;
  if (props !== undefined && (!props || typeof props !== "object" || Array.isArray(props))) return schema;
  if (props && WAKESHIFT_FIELD in (props as object)) return schema;
  return { ...s, properties: { ...((props as object) ?? {}), [WAKESHIFT_FIELD]: { type: "string" } } };
}

// Wakeshift's arguments and output field in each of zod's APIs, for a tool
// whose schema is written in zod and served by a framework that lists it
// itself (Mastra's tool helpers, attachTools' tool): MCP refuses a shape
// that mixes zod 3 and zod 4.
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
const wakeInputs = {
  [SESSION_ARG]: z.string().optional().describe(SESSION_KEY_DESCRIPTION),
  [HARNESS_ARG]: z.string().optional().describe(HARNESS_DESCRIPTION),
  [CONVERSATION_ARG]: z.string().optional().describe(CONVERSATION_DESCRIPTION),
};
const wakeOutput = { wakeshift: z.string().optional().describe(WAKESHIFT_OUTPUT_DESCRIPTION) };

/** @internal Wakeshift's three arguments as a zod shape (Mastra's tool helpers). */
export type WakeInputs = typeof wakeInputs;
/** @internal */
export type WakeInputsV4 = typeof byVersion.v4.inputs;
/** @internal */
export type WakeInputsV3 = typeof byVersion.v3.inputs;
/** @internal */
export type WakeOutputShape = typeof wakeOutput;
/** @internal */
export type WakeOutputV4 = typeof byVersion.v4.output;
/** @internal */
export type WakeOutputV3 = typeof byVersion.v3.output;
/** @internal What a tool's arguments may carry: Wakeshift's three. */
export type WakeArgs = { [K in WakeKey]?: string | undefined };

/** @internal Wakeshift's three arguments as attach's own tools take them, in the zod the host's MCP SDK takes. */
export function wakeInputsFor(host: Host): ZodRawShapeCompat {
  return host.version === 2 || host.zod === (z4 as unknown) ? byVersion.v4.inputs : wakeInputs;
}

// fieldsFor is Wakeshift's fields in the zod API a shape's own fields use.
function fieldsFor(shape: ZodRawShapeCompat, kind: "inputs" | "output", v2: boolean): ZodRawShapeCompat {
  const first = Object.values(shape ?? {})[0];
  const version = first ? (isZ4Schema(first) ? "v4" : "v3") : v2 ? "v4" : null;
  return version ? byVersion[version][kind] : kind === "inputs" ? wakeInputs : wakeOutput;
}

/**
 * @internal A zod object or shape with Wakeshift's fields added (its three
 * arguments, or the output's wakeshift field), in the schema's own zod API,
 * refusing one that already names them: for frameworks that list a tool's
 * zod schema themselves.
 */
export function withWakeFields(schema: ZodRawShapeCompat | AnySchema, kind: "inputs" | "output", v2 = false, keys: readonly WakeKey[] = WAKE_KEYS): unknown {
  const names = kind === "inputs" ? [...WAKE_KEYS] : [WAKESHIFT_FIELD];
  const only = <T extends object>(f: T): T => (kind === "inputs" ? (Object.fromEntries(Object.entries(f).filter(([k]) => (keys as readonly string[]).includes(k))) as T) : f);
  if (isSchema(schema)) {
    const own = getObjectShape(schema);
    if (!own) throw new UsageError("config", "a tool's schema is a zod object or a shape");
    for (const k of names) if (k in own) throw new UsageError("config", `Wakeshift adds ${k} itself: leave it out of the schema`);
    const fields = only(byVersion[isZ4Schema(schema) ? "v4" : "v3"][kind]);
    const s = schema as { extend?: (f: ZodRawShapeCompat) => AnySchema };
    return typeof s.extend === "function" ? s.extend(fields) : z4mini.extend(schema as any, fields as any);
  }
  for (const k of names) if (k in (schema ?? {})) throw new UsageError("config", `Wakeshift adds ${k} itself: leave it out of the shape`);
  return { ...schema, ...only(fieldsFor(schema, kind, v2)) };
}

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

// ── The public types ────────────────────────────────────────────────────

/**
 * What call.subscribe did, by state:
 *
 *   - created, renewed: the calling conversation is subscribed (new, or
 *     renewed). id and user address it later: emit's
 *     `to: [{ user, subscription: id }]`, or `event.end(user, id)`;
 *   - native: the client subscribes itself through MCP Events, and the
 *     block tells it what to subscribe to (after the event's own checks
 *     passed for this caller);
 *   - unavailable: nothing happened, and reason says why: client_unsupported
 *     (an event only the app's tools subscribe to, and a client that only
 *     subscribes itself), no_user (such a client, and no signed-in user) or
 *     refused (the event's authorize refused this caller);
 *   - disabled: Wakeshift is off (no key).
 */
export type Subscription =
  | {
      state: "created" | "renewed";
      id: string;
      /** The user it belongs to (identify's); "" for none. */
      user: string;
      /** The conversation's session. A secret: don't show, store or log it. */
      session: string;
      /** What Wakeshift recognized the agent's program as. */
      recognized: Recognized;
    }
  | { state: "native"; user: string; recognized: Recognized }
  | { state: "unavailable"; reason: "client_unsupported" | "no_user" | "refused" }
  | { state: "disabled" };

/** What call.subscribe did: Subscription's state. */
export type SubscriptionState = Subscription["state"];

// The most replay Wakeshift takes: 24 hours.
const MAX_REPLAY_SECONDS = 86_400;
const DURATION_RE = /^(\d+(?:\.\d+)?)([smhd])$/;
const UNIT_SECONDS: Record<string, number> = { s: 1, m: 60, h: 3600, d: 86_400 };

/** @internal A Duration as seconds (a number is seconds; a string is a number and s, m, h or d), refusing anything else. */
export function durationSeconds(d: unknown, what: string): number {
  if (typeof d === "number" && Number.isFinite(d)) return d;
  const m = typeof d === "string" ? DURATION_RE.exec(d) : null;
  if (!m) throw new UsageError("bad_arguments", `${what} is a number of seconds or a duration like "30m" (a number and s, m, h or d)`);
  return Number(m[1]) * UNIT_SECONDS[m[2]!]!;
}

/** @internal call.subscribe's options as Wakeshift takes them, checked: replay over 0 and at most 24 hours (CR L8), ttl over 0. */
export function subscribeBody(opts: SubscribeOptions): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  if (opts.replay !== undefined) {
    const replay = durationSeconds(opts.replay, "replay");
    if (!(replay > 0 && replay <= MAX_REPLAY_SECONDS)) throw new UsageError("bad_arguments", "replay is over 0 and at most 24 hours");
    body.replay = replay;
  }
  if (opts.ttl !== undefined) {
    const ttl = durationSeconds(opts.ttl, "ttl");
    if (!(ttl > 0)) throw new UsageError("bad_arguments", "ttl is over 0");
    body.ttl = ttl;
  }
  if (opts.once) body.once = true;
  if (opts.reason) body.reason = opts.reason;
  if (opts.label) body.label = opts.label;
  return body;
}

/** How call.subscribe subscribes. */
export interface SubscribeOptions {
  /** End after this long without a delivery or a fresh subscribe (default 30 days). */
  ttl?: Duration;
  /**
   * Also deliver the latest occurrence with these arguments from this far
   * back (at most 24 hours), so one emitted just before the subscribe isn't
   * missed. For a deleteOnDelivery event, only one not yet delivered.
   */
  replay?: Duration;
  /** End after the first delivery: "tell me when this finishes". */
  once?: boolean;
  /** Why the app subscribes the conversation, shown in the console (at most 200 characters). */
  reason?: string;
  /** Names a new session (agent@label), for an app acting for its agents. */
  label?: string;
  /**
   * Your own value for the event's onSubscribe (Subscribed.context): the
   * caller's credential a watch needs, say. It's never sent to Wakeshift
   * and never logged. onSubscribe gets it for this call's subscription, and
   * for a subscription made with the voucher this call handed out while
   * this process still holds it.
   */
  context?: unknown;
}

/** Any of the app's events. emit on one takes `to` as optional, checked at run time. */
export type AnyEvent = WakeEvent<any, any, any, any>;

/** Any of the app's shared (not personal) events: emit takes no `to`. */
export type SharedEvent = WakeEvent<any, any, any, false>;

/** The names of Wakeshift's built-in tools, for code that lists or filters a server's tools. */
export const BUILTIN_TOOL_NAMES = ["subscribe_events", "unsubscribe_events", "list_subscriptions", "wakeshift_wait"] as const;

/** The arguments event takes, as emit and call.subscribe take them. */
export type EventArgs<E> = E extends WakeEvent<any, infer A, any, any> ? SchemaInput<A> : never;

/** The payload event takes, as emit takes it. */
export type EventPayload<E> = E extends WakeEvent<any, any, infer P, any> ? SchemaInput<P> : never;

/**
 * What every ToolCall has, whatever its tool subscribes to: who's calling,
 * unsubscribe and resolveSession. It works until the tool's handler
 * returns; after that its methods throw call_ended.
 */
export interface ToolCallBase<C = unknown> {
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
  /** The conversation's session, set once Wakeshift resolved the conversation (a subscribe that reached it, or resolveSession); never the raw session_key argument, which is only the agent's claim. null before. A secret. */
  readonly session: string | null;
  /** What Wakeshift recognized the agent's program as: null until a subscribe or resolveSession. */
  readonly recognized: Recognized | null;
  /** The call's request context, as your MCP SDK passes it to the tool's handler: 2.x's ctx, 1.x's extra. */
  readonly ctx: C;
  /**
   * Ends the calling conversation's subscription to event with args,
   * resolving to whether one ended. For a native caller it ends the user's
   * own (webhook) subscriptions to the event with those arguments.
   */
  unsubscribe<A extends AnySchema>(event: WakeEvent<string, A, any, any>, args: SchemaInput<A>): Promise<boolean>;
  /**
   * Finds or starts the calling conversation's session without subscribing
   * it (it doesn't make it wakeable). null, with no request to Wakeshift,
   * when nothing names the conversation: no session_key, no conversation_id
   * and no conversation in the call's _meta (a Codex thread).
   */
  resolveSession(): Promise<string | null>;
}

/** call.subscribe, for a tool that subscribes to the events E. */
export interface ToolCallSubscribe<E extends AnyEvent = AnyEvent> {
  /**
   * Subscribes the calling conversation to event with args, or renews its
   * subscription. The reply then carries what the conversation needs to be
   * woken (one block for the call, however many subscribes it makes), and
   * if the call fails, the subscription is ended. Your tool decides who
   * may: authorize isn't consulted for bridged agents. event is one of the
   * events the tool declares (subscribes).
   */
  subscribe<Ev extends E>(event: Ev, args: EventArgs<Ev>, opts?: SubscribeOptions): Promise<Subscription>;
}

/**
 * One call of a tool that subscribes: who's calling, and the calls that
 * subscribe the calling conversation. C is your MCP SDK's request context;
 * E the events the tool declares: call.subscribe takes only those, and a
 * tool that declares none has no call.subscribe.
 */
export type ToolCall<C = unknown, E extends AnyEvent = AnyEvent> = ToolCallBase<C> & ([E] extends [never] ? {} : ToolCallSubscribe<E>);

// ── Printing ────────────────────────────────────────────────────────────

const INSPECT = Symbol.for("nodejs.util.inspect.custom");
const REDACTED = "[redacted]";

/** @internal A subscription whose session doesn't print (it isn't enumerable, and toJSON and inspect redact it). */
export function made(state: "created" | "renewed", id: string, user: string, session: string, recognized: Recognized): Subscription {
  const out = { state, id, user, recognized } as Subscription & { session: string };
  const shown = () => ({ state, id, user, session: REDACTED, recognized });
  Object.defineProperties(out, {
    session: { value: session, enumerable: false },
    toJSON: { value: shown, enumerable: false },
    [INSPECT]: { value: () => shown(), enumerable: false },
  });
  return out;
}

// shownCall is what printing a call shows: who and what, never its
// session or its request context (the caller's credential is in it).
function shownCall(c: { tool: string; user: string | undefined; native: boolean; nativeConversation: string | null; recognized: Recognized | null; session: string | null }) {
  return { tool: c.tool, user: c.user, native: c.native, nativeConversation: c.nativeConversation, recognized: c.recognized, session: c.session ? REDACTED : null };
}

// ── Finding the call ────────────────────────────────────────────────────

// The call each tools/call of a tool that subscribes runs in: found from
// anywhere in its handler's async chain (AsyncLocalStorage), or by its
// request context.
type CallStore = { run<R>(store: CallScope<any>, fn: () => R): R; getStore(): CallScope<any> | undefined };
let callStore: CallStore | undefined;
// AsyncLocalStorage.snapshot, where the runtime has it: onSubscribe for a
// tool's subscription runs in the async context call.subscribe ran in.
let snapshotFn: (() => <R>(fn: () => R) => R) | undefined;

/** @internal A function running fn in the async context it was made in (AsyncLocalStorage.snapshot), if the runtime has one. */
export function snapshotContext(): (<R>(fn: () => R) => R) | undefined {
  try {
    return snapshotFn?.();
  } catch {
    return undefined;
  }
}

/**
 * @internal Loads AsyncLocalStorage, on first attach: through
 * process.getBuiltinModule, so the SDK's module graph loads where node:*
 * modules don't (a bundle), and a runtime without it (Cloudflare Workers
 * without nodejs_compat) hears why.
 */
export function loadCallStore(): void {
  if (callStore) return;
  let mod: { AsyncLocalStorage?: new () => CallStore } | undefined;
  try {
    mod = (globalThis as { process?: { getBuiltinModule?(id: string): unknown } }).process?.getBuiltinModule?.("node:async_hooks") as typeof mod;
  } catch {
    mod = undefined;
  }
  if (!mod?.AsyncLocalStorage) throw new UsageError("config", "attach needs AsyncLocalStorage: on Cloudflare Workers, enable nodejs_compat");
  callStore = new mod.AsyncLocalStorage();
  const snap = (mod.AsyncLocalStorage as unknown as { snapshot?: () => <R>(fn: () => R) => R }).snapshot;
  if (typeof snap === "function") snapshotFn = () => snap.call(mod!.AsyncLocalStorage);
}
const callsByContext = new WeakMap<object, CallScope<any>>();

// Stand-in calls for unit tests (@wakeshift/sdk/testing's withCall): what
// wake.currentCall() returns inside withCall's function.
type TestStore = { run<R>(store: { wake: Wakeshift; call: unknown }, fn: () => R): R; getStore(): { wake: Wakeshift; call: unknown } | undefined };
let testStore: TestStore | undefined;

/** @internal Runs fn with call as wake.currentCall()'s answer (withCall). */
export function runWithTestCall<R>(wake: Wakeshift, call: unknown, fn: () => R): R {
  if (!testStore) {
    const mod = (globalThis as { process?: { getBuiltinModule?(id: string): unknown } }).process?.getBuiltinModule?.("node:async_hooks") as { AsyncLocalStorage?: new () => TestStore } | undefined;
    if (!mod?.AsyncLocalStorage) throw new UsageError("config", "withCall needs AsyncLocalStorage (node:async_hooks)");
    testStore = new mod.AsyncLocalStorage();
  }
  return testStore.run({ wake, call }, fn);
}

/** @internal The stand-in call withCall set for wake, if any. */
export function testCallFor(wake: Wakeshift): unknown {
  const s = testStore?.getStore();
  return s && s.wake === wake ? s.call : undefined;
}

// contextKeys is what a call is found by: the request context its handler
// gets, and 2.x's ctx.mcpReq, which a framework may copy into its own context.
function contextKeys(ctx: unknown): object[] {
  if (!ctx || typeof ctx !== "object") return [];
  const req = (ctx as { mcpReq?: unknown }).mcpReq;
  return req && typeof req === "object" ? [ctx, req] : [ctx];
}

/** @internal The call being answered for wake: the one ctx belongs to, else the one this async chain runs in. */
export function findCall(wake: Wakeshift, ctx?: unknown): CallScope<any> | undefined {
  for (const k of contextKeys(ctx)) {
    const s = callsByContext.get(k);
    if (s && s.owner === wake) return s;
  }
  const s = callStore?.getStore();
  return s && s.owner === wake ? s : undefined;
}

/** @internal The call being answered for wake. Throws no_call outside one, call_ended after it returned. */
export function currentCall(wake: Wakeshift, ctx?: unknown): CallScope<any> {
  const scope = findCall(wake, ctx);
  if (!scope) throw new UsageError("no_call", "currentCall was called outside a tool call that subscribes (one registered through the attachment, or named in wrap)");
  if (scope.ended) throw callEnded(scope.tool);
  return scope;
}

// callEnded is the error for a ToolCall used after its handler returned.
function callEnded(tool: string): UsageError {
  return new UsageError("call_ended", `${tool}'s call already returned, so it can't subscribe, unsubscribe or resolve its session any more`);
}

// ── The block ───────────────────────────────────────────────────────────

// The line a text-carried beacon is on (the core's beacon.Text), which a
// harness's hook reads only as the last line of a result's text.
const BEACON_LINE_PREFIX = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: ";
const CONNECTOR_META_KEY = "wakeshift/connector";
// The most native lines Wakeshift takes for one call.
const MAX_NATIVE_LINES = 20;

/** @internal /calls/addition's answer. */
export interface CallAddition {
  text?: string;
  trailer?: string;
  meta?: Record<string, unknown>;
}

type Content = CallToolResult["content"][number];

// dropTrailers takes every beacon line off the end of the result's last
// text (dropping its block if nothing else is in it). Only Wakeshift's own
// trailer may end a tool's text: a hook acts on the beacon there, and one in
// the app's text may be someone else's, relayed.
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

/** What a tool's handler returns: a string (one text block) or a CallToolResult. */
export type ToolResult = string | CallToolResult;

/**
 * @internal Adds the call's block to a handler's result: Wakeshift's text
 * as a block after the app's content, then a text-carried beacon last, and
 * _meta; a string result stays one text block. Beacon lines ending the
 * app's own text are dropped. A result whose own _meta carries a connector
 * beacon gets no second one. A structured result gets the block's text in
 * its wakeshift field too. test/wrapped-block.json is the shape.
 */
export function mergeBlock(result: ToolResult, add: CallAddition): CallToolResult {
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

// liveKey names a subscription by event and arguments (key order aside).
function liveKey(name: string, args: unknown): string {
  const norm = (v: unknown): unknown =>
    Array.isArray(v) ? v.map(norm) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v as object).sort().map((k) => [k, norm((v as Record<string, unknown>)[k])])) : v;
  return name + "\u0000" + JSON.stringify(norm(args ?? {}));
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

// isInputRequired is whether a result asks the client for input first
// (resultType input_required, or input requests).
function isInputRequired(result: unknown): boolean {
  if (!result || typeof result !== "object") return false;
  const r = result as { resultType?: unknown; inputRequests?: unknown };
  return r.resultType === "input_required" || (!!r.inputRequests && typeof r.inputRequests === "object" && Object.keys(r.inputRequests).length > 0);
}

// isToolResult is whether a handler answered with a tool result (not a task handle).
function isToolResult(result: unknown): result is CallToolResult {
  return isRecord(result) && (Array.isArray(result.content) || result.structuredContent !== undefined || result.isError !== undefined);
}

// ── One call ────────────────────────────────────────────────────────────

/** @internal What a call needs of attach: the client, the server, who's calling. */
export interface AttachContext {
  wake: Wakeshift;
  /** The server, through what attach needs of its MCP SDK (host.ts). */
  host: Host;
  /** Who attach's identify says is calling, or why Wakeshift's part is refused (raw: the MCP SDK's own request context). */
  identify(raw: unknown, tool?: string): Promise<Caller>;
  /** The client's name: the request's (initialize's, else _meta's), else identified (identify's), else "". */
  clientOf(meta: Record<string, unknown> | undefined, identified?: string): string;
  /** attach's override: whether a call comes from a client that subscribes itself (events.native). */
  native?(raw: unknown): boolean;
  /** attach serves the wait tool: only then does a call say its client takes tasks. */
  wait?: boolean;
  /** false: the server serves its own MCP Events, so no caller subscribes natively to Wakeshift's. */
  servesEvents?: boolean;
}

interface CreatedSub {
  event: AnyEvent;
  name: string;
  arguments: unknown;
  session: string;
  pending: string; // the call's pending-addition record, which made it
  // call.subscribe's context, and the async context it ran in: onSubscribe's.
  context: unknown;
  runIn: (<R>(fn: () => R) => R) | undefined;
}

interface NativeLine {
  name: string;
  arguments: unknown;
  // call.subscribe's options, for a tools-only event's line: the core
  // keeps them with the subscribe voucher and applies them when it's
  // redeemed (docs/subscribe-vouchers.md §4.1).
  once?: boolean;
  ttl?: number;
  replay?: number;
}

/**
 * @internal Whether a call comes from a client that subscribes to the app's
 * events itself, on stateless servers too, in this order: the app's
 * override; ChatGPT's _meta["openai/session"]; a client name or User-Agent
 * of a known native client; never a harness Wakeshift wakes itself
 * (BRIDGED_CLIENTS: its name wins over capabilities); a client that declares
 * the MCP Events extension (initialize's capabilities, or _meta's).
 */
export function nativeOf(at: Pick<AttachContext, "host" | "clientOf" | "native" | "servesEvents">, raw: unknown, identified?: string): { native: boolean; conversation: string | null } {
  if (!at.host.servesEvents || at.servesEvents === false) return { native: false, conversation: null };
  const req = at.host.req(raw);
  const meta = (req.meta ?? {}) as Record<string, unknown>;
  const conversation = typeof meta["openai/session"] === "string" && meta["openai/session"] ? (meta["openai/session"] as string) : null;
  if (at.native?.(raw)) return { native: true, conversation };
  if (conversation) return { native: true, conversation };
  if (dialectOf(at.clientOf(meta, identified), userAgentOf(req)) === "chatgpt") return { native: true, conversation: null };
  if (bridgedClient(at.clientOf(meta, identified), userAgentOf(req))) return { native: false, conversation: null };
  const caps = at.host.clientCapabilities();
  if (hasKey(caps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  const metaCaps = meta["io.modelcontextprotocol/clientCapabilities"] as Record<string, unknown> | undefined;
  if (hasKey(metaCaps?.extensions, EXTENSION_KEY)) return { native: true, conversation: null };
  return { native: false, conversation: null };
}

/** @internal One tools/call of a tool that subscribes: what its handler subscribed, for the block and the rollback. */
export class CallScope<C> {
  user: string | undefined;
  native = false;
  nativeConversation: string | null = null;
  recognized: Recognized | null = null;
  // The conversation's session once Wakeshift resolved it (a subscribe or
  // resolveSession). Never the raw session_key argument: that one is the
  // agent's claim, sent on the wire only (#passed).
  #session: string | null = null;
  // The session_key the agent passed, unverified: only for the wire.
  #passed: string | null;
  /** The conversation's session, set once Wakeshift resolved it (a subscribe or resolveSession); never the raw session_key argument. null before. A secret: it doesn't print. */
  get session(): string | null {
    return this.#session;
  }
  /** The request context, as the MCP SDK passed it: it doesn't print. */
  declare readonly ctx: C;

  ended = false;
  pending = "";
  armed = false;
  /** The handler answered with a string: the block keeps it one text block. */
  stringResult = false;
  created: CreatedSub[] = [];
  nativeLines: NativeLine[] = [];
  // Wakeshift's notes from the call's unsubscribes, for the block.
  notes: string[] = [];
  // The context of each native line for a tools-only event (by event and
  // arguments): never sent, only matched to the voucher the core hands out.
  private readonly nativeContexts = new Map<string, unknown>();
  // The call's subscriptions still live, by event and arguments: one the
  // call ended again (call.unsubscribe) leaves it.
  private readonly live = new Set<string>();
  // What each subscribe of the call did, by event and arguments (watch reads it).
  private readonly states = new Map<string, SubscriptionState>();
  /** @internal Watches started in this call for subscriptions it created: stopped if the call fails. */
  watches: { stop(): void }[] = [];
  inflight = new Set<Promise<unknown>>();
  private queue: Promise<unknown> = Promise.resolve();
  // The client's name identify gave, for a request that names none.
  private readonly identified: string | undefined;
  // identify refused Wakeshift's part of the call (failed, or requireUser and no user).
  private readonly refused: Refusal | undefined;
  // The agent the app acts for (identify's agent).
  private readonly agentToken: string | undefined;

  /** @internal The client the call is for. */
  get owner(): Wakeshift {
    return this.at.wake;
  }

  constructor(
    private readonly at: AttachContext,
    readonly tool: string,
    ctx: C,
    private readonly args: { session?: string; harness?: string; conversation?: string },
    readonly declared: readonly AnyEvent[],
    caller: Caller,
  ) {
    Object.defineProperty(this, "ctx", { value: ctx, enumerable: false });
    for (const k of ["at", "args"]) Object.defineProperty(this, k, { enumerable: false });
    this.#passed = args.session || null;
    this.user = caller.ok ? caller.user : undefined;
    this.identified = caller.ok ? caller.client : undefined;
    this.refused = caller.ok ? undefined : caller.reason;
    this.agentToken = caller.ok ? caller.agentToken : undefined;
    const n = nativeOf(at, ctx, this.identified);
    this.native = n.native;
    this.nativeConversation = n.conversation;
  }

  toJSON() {
    return shownCall(this);
  }

  [INSPECT]() {
    return shownCall(this);
  }

  // wire is the call as Wakeshift takes it.
  wire(session = this.#session ?? this.#passed): Call {
    const req = this.at.host.req(this.ctx);
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
      agentToken: this.agentToken,
      // A native caller's transport session binds an anonymous voucher; only
      // to a core that hands them out (an older one refuses fields it doesn't know).
      transportSession: this.native && this.at.wake.vouchersKnown ? transportSessionOf(req) : undefined,
    });
  }

  /**
   * @internal Neutral words for the agent in place of an error, or an error
   * result's text, that passes on this call's identify refusal (the
   * developer's fix stays in the log, identify_error or user_required); "" otherwise.
   */
  refusedWords(shown: unknown): string {
    if (!this.refused) return "";
    const shows = (text: string) => text.includes(fixOf("identify_failed")) || text.includes(fixOf("user_required"));
    const passedOn =
      (shown instanceof UsageError && (shown.code === "identify_failed" || shown.code === "user_required")) ||
      (typeof shown === "string" && shows(shown)) ||
      (shown instanceof Error && shows(shown.message));
    return passedOn ? refusedText(this.refused, true) : "";
  }

  // checkCaller refuses every request of a call identify refused: the
  // vendor's own code still runs, only Wakeshift's part refuses.
  private checkCaller(): void {
    if (this.refused === "anonymous") throw new UsageError("user_required", `requireUser refused ${this.tool}'s caller: identify gave no user`);
    if (this.refused) throw new UsageError("identify_failed", `identify threw, failed or took too long for ${this.tool}'s call, so it can't subscribe`);
  }

  // signal is the MCP request's own abort signal: call.subscribe follows it.
  private get signal(): AbortSignal | undefined {
    return this.at.host.req(this.ctx).signal;
  }

  // request runs one request of the call to Wakeshift, after any before it
  // (one at a time per call), refusing once the handler has returned.
  private request<T>(fn: () => Promise<T>): Promise<T> {
    if (this.ended) return Promise.reject(callEnded(this.tool));
    const run = this.queue.then(() => (this.ended ? Promise.reject(callEnded(this.tool)) : fn()));
    this.queue = run.catch(() => undefined);
    this.inflight.add(run);
    run.then(
      () => this.inflight.delete(run),
      () => this.inflight.delete(run),
    );
    return run;
  }

  private checkEvent(event: AnyEvent): void {
    if (!event || typeof event.owner !== "function" || event.owner() !== this.at.wake) {
      throw new UsageError("unknown_event", `${event?.name ?? "that"} is another app's event: this app doesn't offer it`);
    }
  }

  private checkDeclared(event: AnyEvent): void {
    if (this.declared.some((e) => e === event)) return;
    throw new UsageError("undeclared_subscribe", `${this.tool} subscribed to ${event.name}, which isn't in its subscribes`);
  }

  subscribe(event: AnyEvent, args: unknown, opts: SubscribeOptions = {}): Promise<Subscription> {
    return this.request(async () => {
      this.checkEvent(event);
      this.checkDeclared(event);
      this.checkCaller();
      const known = event.parseArgs(args);
      const options = subscribeBody(opts);
      if (this.native) return this.subscribeNative(event, known, options, opts.context);
      if (event.personal && !this.user) {
        throw new UsageError("bad_arguments", `${event.name} is personal: call.subscribe needs a user (attach's identify), who the subscription belongs to`);
      }
      const body: Record<string, unknown> = { call: this.wire(), name: event.name, arguments: known, defer_addition: true, ...options };
      if (this.pending) body.pending = this.pending;
      const out = await this.at.wake.requestApp<{
        event_subscription: { id: string; user?: string; created: boolean };
        session: string;
        agent?: Partial<Recognized>;
        pending?: string;
      }>("POST", "/event-subscriptions", body, { signal: this.signal });
      if (out.pending) this.pending = out.pending;
      this.armed = true;
      this.#session = out.session;
      const rec = recognizedOf(out.agent);
      this.recognized = rec;
      const es = out.event_subscription;
      const runIn = snapshotContext();
      if (es.created) this.created.push({ event, name: event.name, arguments: known, session: out.session, pending: out.pending ?? this.pending, context: opts.context, runIn });
      else {
        // A later subscribe of the call to the same event and arguments: its context is onSubscribe's.
        const c = this.created.find((x) => x.name === event.name && sameJSON(x.arguments, known));
        if (c) Object.assign(c, { context: opts.context, runIn });
      }
      this.live.add(liveKey(event.name, known));
      this.states.set(liveKey(event.name, known), es.created ? "created" : "renewed");
      return made(es.created ? "created" : "renewed", es.id, es.user ?? this.user ?? "", out.session, rec);
    });
  }

  // subscribeNative is call.subscribe for a client that subscribes itself
  // (MCP Events): the block names the event for it to subscribe to, only
  // if its follow-up would be allowed (design §3, SEC M5). Wakeshift isn't
  // asked here.
  private async subscribeNative(event: AnyEvent, args: Record<string, unknown>, options: Record<string, unknown> = {}, context?: unknown): Promise<Subscription> {
    const unavailable = (reason: "client_unsupported" | "no_user" | "refused"): Subscription => {
      this.states.set(liveKey(event.name, args), "unavailable");
      this.at.wake.log.log("native_unavailable", `${this.tool} couldn't subscribe this MCP Events client to ${event.name} (${reason})`, { tool: this.tool, event: event.name, reason });
      return { state: "unavailable", reason };
    };
    if (!event.open) {
      // A tools-only event: while the core hands out subscribe vouchers, the
      // line carries one, so the client subscribes with what the tool gave
      // it, signed in or not (docs/subscribe-vouchers.md §7.1). Not for an
      // app acting for its agent, which the core gives none.
      if (this.agentToken || !(await this.at.wake.vouchersOffered())) return unavailable("client_unsupported");
      const line: Partial<NativeLine> = {};
      if (options.once === true) line.once = true;
      if (typeof options.ttl === "number") line.ttl = options.ttl;
      if (typeof options.replay === "number") line.replay = options.replay;
      this.addNative(event.name, args, line);
      this.nativeContexts.set(liveKey(event.name, args), context);
      this.states.set(liveKey(event.name, args), "native");
      return { state: "native", user: this.user ?? "", recognized: recognizedOf(undefined) };
    }
    if (!this.user) return unavailable("no_user");
    if (event.gated) {
      const req = this.at.host.req(this.ctx);
      const client = this.at.clientOf(req.meta, this.identified);
      if (!(await event.allows(subscriberOf(this.user, client, true, req, this.ctx), args))) return unavailable("refused");
    }
    this.addNative(event.name, args);
    this.states.set(liveKey(event.name, args), "native");
    return { state: "native", user: this.user, recognized: recognizedOf(undefined) };
  }

  unsubscribe(event: AnyEvent, args: unknown): Promise<boolean> {
    return this.request(async () => {
      this.checkEvent(event);
      this.checkCaller();
      const known = event.parseArgs(args);
      if (this.native) return this.endNative(event.name, known);
      const out = await this.at.wake.requestApp<{ ended?: boolean; addition?: CallAddition }>(
        "POST",
        "/event-subscriptions/unsubscribe",
        { call: this.wire(), name: event.name, arguments: known },
        { signal: this.signal },
      );
      // addition: the call passed no session key, so it ended nothing; the
      // block says to call again with it.
      const note = out.addition?.text?.trim();
      if (note && !this.notes.includes(note)) this.notes.push(note);
      if (out.ended) this.live.delete(liveKey(event.name, known));
      return !!out.ended;
    });
  }

  resolveSession(): Promise<string | null> {
    return this.request(async () => {
      this.checkCaller();
      if (this.pending && this.session) return this.session;
      const wire = this.wire();
      // Nothing names the conversation: the core would only start a new one.
      if (!wire.session && !wire.conversation && !namesConversation(wire.meta) && !metaString(wire.meta, "threadId")) return null;
      const out = await this.at.wake.requestApp<{ session: string; pending: string; agent?: Partial<Recognized> }>("POST", "/calls/session", { call: wire }, { signal: this.signal });
      this.pending = out.pending;
      this.#session = out.session;
      this.recognized = recognizedOf(out.agent);
      return out.session;
    });
  }

  // endNative ends the user's webhook subscriptions to event with these
  // arguments, and drops the call's own native line for them.
  private async endNative(name: string, args: unknown): Promise<boolean> {
    this.nativeLines = this.nativeLines.filter((l) => !(l.name === name && sameJSON(l.arguments, args)));
    this.nativeContexts.delete(liveKey(name, args));
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
  private addNative(name: string, args: unknown, options: Partial<NativeLine> = {}): void {
    if (this.nativeLines.some((l) => l.name === name && sameJSON(l.arguments, args))) return;
    if (this.nativeLines.length === MAX_NATIVE_LINES) return; // the core takes at most MAX_NATIVE_LINES
    this.nativeLines.push({ name, arguments: args, ...options });
  }

  // end closes the call to further requests, after waiting for any in flight.
  async end(): Promise<void> {
    if (this.inflight.size > 0) {
      this.at.wake.log.log("subscribe_in_flight", `${this.tool} returned while a request of its call was in flight; the SDK waited for it`, { tool: this.tool });
      await Promise.allSettled([...this.inflight]);
    }
    this.ended = true;
  }

  /** @internal What the call's subscribe for this event and arguments did (watchKey), if it subscribed. */
  stateOf(key: string): SubscriptionState | undefined {
    return this.states.get(key);
  }

  /** @internal After the call succeeded: onSubscribe for each subscription it created that's still live. */
  announce(): void {
    for (const c of this.created) {
      if (this.live.has(liveKey(c.name, c.arguments))) c.event.subscribed({ args: c.arguments, user: this.user, via: "tool", tool: this.tool, ...(c.context === undefined ? {} : { context: c.context }) }, c.runIn);
    }
  }

  // rollback ends every subscription the call created and no other call
  // has renewed since, with reason rolled_back, whatever became of the request.
  async rollback(): Promise<void> {
    for (const w of this.watches.splice(0)) w.stop();
    for (const c of this.created) {
      try {
        await this.at.wake.requestApp("POST", "/event-subscriptions/unsubscribe", { call: this.wire(c.session), name: c.name, arguments: c.arguments, reason: "rolled_back", pending: c.pending });
      } catch {
        this.at.wake.log.log("rollback_failed", `ending a failed call's subscription failed (${this.tool}, ${c.name})`, { tool: this.tool, event: c.name });
      }
    }
    this.created = [];
  }

  // addition asks Wakeshift for the call's block. A call that names no
  // conversation and subscribed nothing gets nothing (null). Only a failure
  // for a call that subscribed is thrown. Notes from the call's
  // unsubscribes come first.
  async addition(): Promise<CallAddition | null> {
    // A call that subscribed and then ended every subscription it made gets
    // no block: there's nothing to be woken for (only its notes, if any).
    const ended = this.armed && this.live.size === 0 && this.nativeLines.length === 0;
    const add = ended ? null : await this.fetchAddition();
    if (this.notes.length === 0) return add;
    const text = [this.notes.join(" "), add?.text?.trim() ?? ""].filter(Boolean).join("\n\n");
    return { ...(add ?? {}), text };
  }

  private async fetchAddition(): Promise<CallAddition | null> {
    // The manifest's answer first, so a native call's wire knows whether the core hands out vouchers.
    if (this.native) await this.at.wake.vouchersOffered();
    const wire = this.wire();
    if (!this.pending && this.nativeLines.length === 0 && !wire.session && !wire.conversation && !namesConversation(wire.meta)) return null;
    const body: Record<string, unknown> = { call: wire };
    if (this.pending) body.pending = this.pending;
    if (this.nativeLines.length > 0) body.native = this.nativeLines;
    try {
      const add = await this.at.wake.requestApp<CallAddition>("POST", "/calls/addition", body);
      // The vouchers handed out for the call's tools-only lines: each
      // remembered (by its hash) with its subscribe's context.
      if (this.nativeContexts.size > 0) {
        rememberVouchers(
          this.at.wake,
          add?.text,
          this.nativeLines.map((l) => ({ name: l.name, arguments: l.arguments, context: this.nativeContexts.get(liveKey(l.name, l.arguments)) })),
        );
      }
      return add;
    } catch (err) {
      // A courtesy (the call subscribed nothing): nothing is lost, so
      // nothing is logged, as in every SDK.
      if (this.armed || this.nativeLines.length > 0) throw err;
      return null;
    }
  }
}

/**
 * @internal The call a disabled client's tools get (no key): it runs the
 * same local checks as an enabled one and subscribes nothing.
 */
export class DisabledCall<C> {
  readonly user: string | undefined = undefined;
  readonly native = false;
  readonly nativeConversation: string | null = null;
  readonly session: string | null = null;
  readonly recognized: Recognized | null = null;
  declare readonly ctx: C;

  constructor(
    private readonly wake: Wakeshift,
    readonly tool: string,
    ctx: C,
    private readonly declared: readonly AnyEvent[] | null,
  ) {
    Object.defineProperty(this, "ctx", { value: ctx, enumerable: false });
    for (const k of ["wake", "declared"]) Object.defineProperty(this, k, { enumerable: false });
  }

  toJSON() {
    return shownCall(this);
  }

  [INSPECT]() {
    return shownCall(this);
  }

  private check(event: AnyEvent, args: unknown): void {
    if (!event || typeof event.owner !== "function" || event.owner() !== this.wake) {
      throw new UsageError("unknown_event", `${event?.name ?? "that"} is another app's event: this app doesn't offer it`);
    }
    event.parseArgs(args);
  }

  async subscribe(event: AnyEvent, args: unknown, opts: SubscribeOptions = {}): Promise<Subscription> {
    this.check(event, args);
    if (this.declared && !this.declared.some((e) => e === event)) {
      throw new UsageError("undeclared_subscribe", `${this.tool} subscribed to ${event.name}, which isn't in its subscribes`);
    }
    subscribeBody(opts);
    return { state: "disabled" };
  }

  async unsubscribe(event: AnyEvent, args: unknown): Promise<boolean> {
    this.check(event, args);
    return false;
  }

  async resolveSession(): Promise<string | null> {
    throw new UsageError("disabled", "Wakeshift is off (no app key), so there's no session to resolve");
  }
}

// ── The protocol layer ──────────────────────────────────────────────────

/** @internal The names of Wakeshift's own tools: their calls declare its arguments. */
export const OWN_TOOLS = new Set<string>(BUILTIN_TOOL_NAMES);

// withoutProps is a JSON Schema without the named properties.
function withoutProps(schema: unknown, names: string[]): unknown {
  const s = schema as { properties?: Record<string, unknown> } | undefined;
  if (!s?.properties) return schema;
  return { ...s, properties: Object.fromEntries(Object.entries(s.properties).filter(([k]) => !names.includes(k))) };
}

// Claude Code's cap on a tool's description.
const MAX_DESCRIPTION = 2048;

/**
 * @internal Wakeshift's part of tools/list and tools/call on one server:
 * the tools that subscribe (registered through the attachment, or named in
 * wrap) and the layers that strip, bind and amend.
 */
export class ToolLayer<C = unknown> {
  // The tools that subscribe, by name, with the events each declares.
  private readonly subscribing = new Map<string, readonly AnyEvent[]>();
  // The names wrap gave (checked against tools/list: CR L2).
  private readonly wrapNames = new Set<string>();
  // wrap as a function: the events of a tool named at run time.
  private resolve?: (tool: string) => readonly AnyEvent[] | undefined;
  // Tools whose own schema carries Wakeshift's arguments by design (Mastra's
  // tool helpers): their calls keep them, and tools/list adds none.
  private readonly selfArgs = new Set<string>();
  // Tools that subscribe whose own schema names one of Wakeshift's
  // arguments: served unwrapped (CR L4).
  private readonly conflicted = new Set<string>();
  // Each listed tool's own input properties, as tools/list last gave them:
  // a call keeps the wake arguments its tool names itself.
  private readonly ownProps = new Map<string, Set<string>>();
  // attach's own tools' output schemas, by name, for tools/list.
  private readonly ownOutput = new Map<string, Record<string, unknown>>();
  // How tools/list gives a tool, after the MCP SDK listed it (subscribe_events' enum).
  private readonly patches = new Map<string, (tool: Record<string, unknown>) => Record<string, unknown>>();

  // Built-ins tools/list leaves out while nothing on the server can make a
  // subscription (no tool declared with subscribes, no event open to
  // subscribe_events): list_subscriptions and unsubscribe_events.
  private readonly idleBuiltins = new Set<string>();

  constructor(readonly at: AttachContext) {}

  /** @internal Built-ins tools/list leaves out while no tool on the server subscribes. */
  hideWhenIdle(names: readonly string[]): void {
    for (const n of names) this.idleBuiltins.add(n);
  }

  /** @internal Changes how tools/list gives one of attach's own tools. */
  patchListed(name: string, patch: (tool: Record<string, unknown>) => Record<string, unknown>): void {
    this.patches.set(name, patch);
  }

  /** Declares name a tool that subscribes to events: named in wrap (fromWrap), or with Wakeshift's arguments in its own schema (selfArgs). */
  declare(name: string, events: readonly AnyEvent[], opts: { fromWrap?: boolean; selfArgs?: boolean } = {}): void {
    this.subscribing.set(name, events);
    if (opts.fromWrap) this.wrapNames.add(name);
    if (opts.selfArgs) this.selfArgs.add(name);
  }

  /** Names the tools that subscribe as tools/list and tools/call reach them: wrap given as a function, for tools named at run time. */
  wrapWith(fn: (tool: string) => readonly AnyEvent[] | undefined): void {
    this.resolve = fn;
  }

  // eventsOf is the events a tool subscribes to: declared, else what wrap's
  // function says (asked each time, so its answer may change).
  private eventsOf(name: string): readonly AnyEvent[] | undefined {
    const declared = this.subscribing.get(name);
    if (declared || !this.resolve || OWN_TOOLS.has(name)) return declared;
    const list = this.resolve(name);
    if (list === undefined || list === null) return undefined;
    try {
      return checkSubscribes(this.at.wake, name, list);
    } catch {
      // Asked while serving: a mistake is logged and the tool served unwrapped.
      this.at.wake.log.log("wrap_bad_event", `wrap's function gave ${name} something other than a list of this client's events, so it's served unwrapped`, { tool: name });
      return undefined;
    }
  }

  /** The events name declares, if it's a tool that subscribes (and isn't served unwrapped). */
  declared(name: string): readonly AnyEvent[] | undefined {
    return this.conflicted.has(name) ? undefined : this.eventsOf(name);
  }

  /** attach's own tools' output schemas (JSON), for tools/list to give them. */
  ownOutputSchemas(schemas: Record<string, Record<string, unknown>>): void {
    for (const [name, s] of Object.entries(schemas)) this.ownOutput.set(name, s);
  }

  /** Installs the tools/call and tools/list layers on the server's handlers. */
  install(): void {
    const handlers = this.at.host.handlers();
    if (!handlers) return; // a future SDK: attach logged it
    wrapMethod(handlers, "tools/call", (inner) => (inner ? (request: any, raw: unknown) => this.call(request, raw, inner) : undefined));
    wrapMethod(handlers, "tools/list", (inner) =>
      inner
        ? async (request: any, raw: unknown) => {
            const out = await inner(request, raw);
            if (out && Array.isArray(out.tools)) await this.amend(out.tools, request?.params?.cursor === undefined && out.nextCursor === undefined);
            return out;
          }
        : undefined,
    );
  }

  // keeps is whether the tool named name declares the wake argument key itself.
  private keeps(name: string, key: string): boolean {
    if (OWN_TOOLS.has(name) || this.selfArgs.has(name)) return true;
    const registered = this.at.host.registeredTool(name);
    if (registered) return propsOf(registered.inputSchema).includes(key);
    return this.ownProps.get(name)?.has(key) ?? false;
  }

  /** @internal Arguments without the wake arguments the tool doesn't declare, and the three as the call carried them. */
  strip(name: string, args: unknown): { args: unknown; wake: { session?: string; harness?: string; conversation?: string } } {
    if (!isRecord(args)) return { args, wake: {} };
    const str = (k: string) => (typeof args[k] === "string" && args[k] ? (args[k] as string) : undefined);
    const wake = { session: str(SESSION_ARG), harness: str(HARNESS_ARG), conversation: str(CONVERSATION_ARG) };
    if (!WAKE_KEYS.some((k) => k in args)) return { args, wake };
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(args)) if (!(WAKE_KEYS as readonly string[]).includes(k) || this.keeps(name, k)) out[k] = v;
    return { args: out, wake };
  }

  /** @internal A call's scope, once identify has answered. */
  async begin(name: string, wakeArgs: { session?: string; harness?: string; conversation?: string }, raw: unknown, declared: readonly AnyEvent[]): Promise<CallScope<C>> {
    const caller = await this.at.identify(raw, name);
    return new CallScope<C>(this.at, name, raw as C, wakeArgs, declared, caller);
  }

  // call is the tools/call layer.
  private async call(request: any, raw: unknown, inner: Handler): Promise<any> {
    const name = request?.params?.name;
    if (typeof name !== "string" || OWN_TOOLS.has(name)) return inner(request, raw);
    const { args, wake } = this.strip(name, request.params.arguments);
    const stripped = args === request.params.arguments ? request : { ...request, params: { ...request.params, arguments: args } };
    const declared = this.declared(name);
    if (!declared) return inner(stripped, raw);
    const scope = await this.begin(name, wake, raw, declared);
    return this.run(scope, raw, () => inner(stripped, raw));
  }

  /** @internal Runs fn (the call's inner handler) as scope's call, then finishes it: the block, or the rollback. */
  async run(scope: CallScope<C>, raw: unknown, fn: () => Promise<unknown>): Promise<any> {
    const keys = contextKeys(raw);
    for (const k of keys) callsByContext.set(k, scope);
    let result: unknown;
    try {
      result = await (callStore ? callStore.run(scope, fn) : fn());
    } catch (err) {
      await this.fail(scope);
      const neutral = scope.refusedWords(err);
      if (neutral) return { isError: true, content: [{ type: "text", text: neutral }] };
      throw err;
    } finally {
      for (const k of keys) if (callsByContext.get(k) === scope) callsByContext.delete(k);
    }
    return this.finish(scope, result);
  }

  /** @internal Ends a call whose handler threw: what it subscribed is rolled back. */
  async fail(scope: CallScope<C>): Promise<void> {
    await scope.end();
    await scope.rollback();
  }

  /**
   * @internal The second half of a call: the result with Wakeshift's block,
   * or rolled back (a failed result, a canceled call, an answer that isn't
   * final). The MCP SDK has already validated the result against the
   * tool's output schema: Wakeshift's block comes after.
   */
  async finish(scope: CallScope<C>, result: unknown): Promise<any> {
    await scope.end();
    if (!isToolResult(result)) {
      // Not a tool's answer (a task handle) or one that asks for input
      // first: no block to add, so nothing the call subscribed stays.
      const subscribed = scope.created.length > 0 || scope.armed || scope.nativeLines.length > 0;
      await scope.rollback();
      if (subscribed && !isInputRequired(result)) {
        this.at.wake.log.log("not_a_tool_result", `${scope.tool} answered with something other than a tool result (a task?), so what it subscribed was rolled back`, { tool: scope.tool });
      }
      return result;
    }
    if (result.isError || this.at.host.req(scope.ctx).signal?.aborted || isInputRequired(result)) {
      await scope.rollback();
      const shown = (result.content ?? []).map((c: any) => (c?.type === "text" ? String(c.text) : "")).join("\n");
      const neutral = result.isError ? scope.refusedWords(shown) : "";
      if (neutral) return { isError: true, content: [{ type: "text", text: neutral }] };
      return result;
    }
    let add: CallAddition | null = null;
    try {
      add = await scope.addition();
      scope.announce();
    } catch (err) {
      if (!scope.armed && scope.created.length === 0 && scope.nativeLines.length > 0) {
        // Only native lines, so nothing to roll back: the vendor's result
        // goes out as it would without Wakeshift (docs/subscribe-vouchers.md §7.2).
        this.at.wake.log.log("native_unavailable", `Wakeshift's addition to a result of ${scope.tool} failed, so it carries no line for the MCP Events client`, {
          tool: scope.tool,
          event: scope.nativeLines[0]!.name,
          reason: "addition_failed",
        });
        const plain = scope.stringResult && result.content.length === 1 && result.content[0]!.type === "text" && result.structuredContent === undefined;
        return mergeBlock(plain ? (result.content[0] as { text: string }).text : result, {});
      }
      // Wakeshift couldn't give the block: the result goes out as the tool
      // gave it, as it would without Wakeshift, never an error (the tool may
      // have done paid or visible work). The conversation wouldn't learn how
      // it's woken, so what the call subscribed is rolled back.
      await scope.rollback();
      this.at.wake.log.log("block_failed", `Wakeshift's addition to a result of ${scope.tool} failed (${errorType(err)}), so the result went out without it and the call's subscriptions were rolled back`, { tool: scope.tool, error_type: errorType(err) });
    }
    // A string the handler returned stays one text block.
    const one = scope.stringResult && result.content.length === 1 && result.content[0]!.type === "text" && result.structuredContent === undefined;
    return mergeBlock(one ? (result.content[0] as { text: string }).text : result, add ?? {});
  }

  // amend is the tools/list layer: each tool that subscribes gets
  // Wakeshift's arguments, the subscribes sentence and the wakeshift output
  // field; attach's own tools get their output schemas. A tool that
  // subscribes whose own schema names one of Wakeshift's arguments is left
  // unwrapped (CR L4), and a name in wrap the server never lists is logged
  // (CR L2): both checked on every tools/list, since tools change (SEC L6).
  private async amend(tools: unknown[], complete: boolean): Promise<void> {
    const handlers = this.at.host.handlers();
    // A tools/call handler someone replaced around Wakeshift: its arguments
    // aren't stripped, so they aren't advertised.
    const callWrapped = !handlers || stillWrapped(handlers, "tools/call");
    const listed = new Set<string>();
    let named = false;
    // Something subscribes: a tool declared with events (subscribes: [] doesn't count).
    let subscribes = [...this.subscribing.values()].some((e) => e.length > 0);
    for (let i = 0; i < tools.length; i++) {
      const t = tools[i] as { name?: unknown; description?: unknown; inputSchema?: unknown; outputSchema?: unknown } | null;
      if (!t || typeof t !== "object" || typeof t.name !== "string") continue;
      listed.add(t.name);
      if (!OWN_TOOLS.has(t.name)) this.ownProps.set(t.name, new Set(propsOf(t.inputSchema)));
      const patch = this.patches.get(t.name);
      const own = this.ownOutput.get(t.name);
      if (OWN_TOOLS.has(t.name) && (patch || (own && !t.outputSchema))) {
        let out: Record<string, unknown> = { ...t };
        if (own && !t.outputSchema) out.outputSchema = own;
        if (patch) out = patch(out);
        tools[i] = out;
        continue;
      }
      const events = this.eventsOf(t.name);
      if (!events || OWN_TOOLS.has(t.name)) continue;
      if (events.length > 0) subscribes = true;
      const self = this.selfArgs.has(t.name);
      if (!self && namesWakeArgs(t.inputSchema, t.outputSchema)) {
        this.conflicted.add(t.name);
        this.at.wake.log.log("wrap_arg_conflict", `${t.name} subscribes, but its tools/list entry already has session_key, harness, conversation_id or wakeshift in its inputSchema or outputSchema, so it's served unwrapped`, { tool: t.name });
        continue;
      }
      this.conflicted.delete(t.name);
      const out: Record<string, unknown> = { ...t };
      // The compact tools/list: all three for a tool that subscribes to
      // something, only session_key for one that subscribes to nothing.
      if (callWrapped && !self) out.inputSchema = withWakeArgsJson(t.inputSchema, events.length > 0 ? WAKE_KEYS : [SESSION_ARG]);
      // A schema with the three in it already (Mastra's attached.inputs)
      // lists only session_key when the tool subscribes to nothing; it
      // still accepts all three.
      else if (self && events.length === 0) out.inputSchema = withoutProps(t.inputSchema, [HARNESS_ARG, CONVERSATION_ARG]);
      if (t.outputSchema) out.outputSchema = withWakeshiftField(t.outputSchema);
      if (events.length > 0) {
        const d = typeof t.description === "string" ? t.description : "";
        out.description = withSubscribes(d, events.map((e) => e.description));
        named = true;
      }
      tools[i] = out;
    }
    // Built-in tools carry execution only when the server's own tools do
    // (an MCP SDK that adds it to every tool): a strict tool-list check sees
    // one shape. The wait tool keeps what its MCP SDK gives it (tasks).
    if (!tools.some((t) => isRecord(t) && typeof t.name === "string" && !OWN_TOOLS.has(t.name) && t.execution !== undefined)) {
      for (let i = 0; i < tools.length; i++) {
        const t = tools[i];
        if (isRecord(t) && typeof t.name === "string" && OWN_TOOLS.has(t.name) && t.name !== "wakeshift_wait" && "execution" in t) {
          const { execution: _execution, ...rest } = t;
          tools[i] = rest;
        }
      }
    }
    if (!subscribes && this.idleBuiltins.size > 0) {
      for (let i = tools.length - 1; i >= 0; i--) {
        const n = (tools[i] as { name?: unknown } | null)?.name;
        if (typeof n === "string" && this.idleBuiltins.has(n)) tools.splice(i, 1);
      }
    }
    if (named) await this.nameApp(tools);
    // Claude Code keeps a tool's first 2048 characters: past that, Wakeshift's sentence is cut off.
    for (const t of tools as { name?: unknown; description?: unknown }[]) {
      if (typeof t?.name === "string" && typeof t.description === "string" && t.description.length > MAX_DESCRIPTION && !OWN_TOOLS.has(t.name) && !this.conflicted.has(t.name) && (this.eventsOf(t.name)?.length ?? 0) > 0) {
        this.at.wake.log.log("description_too_long", `${t.name}'s description is ${t.description.length} characters with Wakeshift's sentence`, { tool: t.name });
      }
    }
    if (complete) {
      for (const name of this.wrapNames) {
        if (!listed.has(name)) this.at.wake.log.log("wrap_unknown_tool", `wrap names ${name}, which the server doesn't list`, { tool: name });
      }
    }
  }

  // nameApp names the app in the subscribes sentences, as the console has
  // it (GET /app, asked once); a name Wakeshift doesn't give in time leaves "this app".
  private async nameApp(tools: unknown[]): Promise<void> {
    const [first, second] = unnamedNotifiesParts();
    const has = (t: unknown) => {
      const d = (t as { description?: unknown } | null)?.description;
      return typeof d === "string" && d.includes(first) && d.includes(second);
    };
    if (!tools.some(has)) return;
    const name = await this.at.wake.appName();
    if (!name) return;
    const [named1, named2] = [first.replace(UNNAMED_APP, name), second.replace("This app", name)];
    for (let i = 0; i < tools.length; i++) {
      const t = tools[i] as { description: string };
      if (has(t)) tools[i] = { ...t, description: t.description.replace(first, named1).replace(second, named2) };
    }
  }
}

// ── The attachment ──────────────────────────────────────────────────────

/** The input a tool's schema takes, as its handler gets it: a Standard Schema's output (zod, ArkType, Valibot), or a zod shape's. */
export type InferArgs<I> = I extends { "~standard": { types?: infer T } }
  ? NonNullable<T> extends { output: infer O }
    ? O
    : Record<string, unknown>
  : I extends ZodRawShapeCompat
    ? ShapeOutput<I>
    : Record<string, unknown>;

/**
 * A tool that subscribes, as attached.registerTool takes it: the host
 * SDK's own registerTool config (title, description, inputSchema,
 * outputSchema, annotations, _meta, and any other field it takes, passed
 * through unchanged), plus subscribes, the events it subscribes the calling
 * conversation to (leave it out, or [], for none: a plain tool, whose
 * handler can still call.unsubscribe and resolveSession).
 */
export interface RegisterToolConfig<I = unknown, O = unknown, E extends readonly AnyEvent[] = readonly AnyEvent[]> {
  title?: string;
  description?: string;
  /** Whatever your MCP SDK takes: a zod object or shape, or (2.x) any Standard Schema. Wakeshift's arguments are added in tools/list, not here. */
  inputSchema?: I;
  outputSchema?: O;
  annotations?: Record<string, unknown>;
  _meta?: Record<string, unknown>;
  /** The events the tool subscribes to: call.subscribe takes only these, and tools/list says the tool wakes the agent on them. Left out: none, as []. */
  subscribes?: E;
  [field: string]: unknown;
}

/**
 * What a tool's handler returns: a string, or a CallToolResult. With an
 * outputSchema, a CallToolResult, exactly as the MCP SDK's own registerTool
 * takes it (its structuredContent is checked against the schema at run
 * time, by the MCP SDK); a string has no structuredContent, so it's refused.
 */
export type RegisterToolResult<O> = unknown extends O ? string | CallToolResult : CallToolResult;

type RegisteredOf<S> = S extends { registerTool(...args: any[]): infer R } ? R : unknown;

/**
 * The handler of a tool that subscribes: your MCP SDK's own handler with
 * call appended, `(args, ctx, call)`, or `(ctx, call)` for a tool without
 * an inputSchema.
 */
export type RegisterToolHandler<C, I, O, E extends AnyEvent> = 0 extends 1 & I
  ? // An inputSchema typed any takes args, as one with a schema does.
    (args: any, ctx: C, call: ToolCall<C, E>) => RegisterToolResult<O> | Promise<RegisterToolResult<O>>
  : [I] extends [undefined]
    ? (ctx: C, call: ToolCall<C, E>) => RegisterToolResult<O> | Promise<RegisterToolResult<O>>
    : (args: InferArgs<I>, ctx: C, call: ToolCall<C, E>) => RegisterToolResult<O> | Promise<RegisterToolResult<O>>;

/**
 * Wakeshift attached to one MCP server: the handle `wake.attach` returns.
 * S is the server; its handlers' request context is `ctx` in your tools.
 */
export class Attachment<S = unknown> {
  /** @internal Use wake.attach. */
  constructor(
    private readonly wake: Wakeshift,
    private readonly server: S,
    private readonly layer: ToolLayer<HostContext<S>> | null,
  ) {
    for (const k of ["wake", "server", "layer"]) Object.defineProperty(this, k, { enumerable: false });
  }

  toJSON(): Record<string, never> {
    return {};
  }

  [INSPECT](): string {
    return "Attachment {}";
  }

  /**
   * Registers a tool that subscribes, on the server passed to attach, with
   * your MCP SDK's own registerTool: the config is that SDK's, plus
   * subscribes; the handler is that SDK's, with call appended. Returns the
   * SDK's RegisteredTool. tools/list gives the tool Wakeshift's arguments,
   * a sentence saying it wakes the agent on the events it subscribes to,
   * and the wakeshift field in its output schema; its calls reach your
   * handler without those arguments, and its results get Wakeshift's block.
   *
   *   attached.registerTool("watch_ci", { description: "Watch CI.", inputSchema: z.object({ repo: z.string() }), subscribes: [ciFinished] },
   *     async ({ repo }, ctx, call) => { await call.subscribe(ciFinished, { repo }); return `Watching ${repo}.` })
   */
  registerTool<const E extends readonly AnyEvent[] = readonly [], I = undefined, O = unknown>(
    name: string,
    config: RegisterToolConfig<I, O, E>,
    handler: RegisterToolHandler<HostContext<S>, I, O, E[number]>,
  ): RegisteredOf<S> {
    return logConfigError(this.wake.log, () => this.register(name, config, handler as unknown as (...args: unknown[]) => ToolResult | Promise<ToolResult>)) as RegisteredOf<S>;
  }

  private register(name: string, config: RegisterToolConfig<unknown, unknown, readonly AnyEvent[]>, handler: (...args: unknown[]) => ToolResult | Promise<ToolResult>): unknown {
    if (!name || typeof name !== "string") throw new UsageError("config", "registerTool needs the tool's name");
    if (OWN_TOOLS.has(name)) throw new UsageError("config", `registerTool can't register ${name}: Wakeshift's own tool has that name`);
    if (!config || typeof config !== "object") throw new UsageError("config", `registerTool("${name}", config, handler): config is your MCP SDK's tool config, with subscribes`);
    if (typeof handler !== "function") throw new UsageError("config", `registerTool("${name}", config, handler) needs a handler`);
    const { subscribes, ...hostConfig } = config;
    // Left out is [] (a plain tool): never an error, with or without a key.
    const events = checkSubscribes(this.wake, name, subscribes ?? []);
    if (propsOf(config.inputSchema).some((k) => (WAKE_KEYS as readonly string[]).includes(k))) {
      throw new UsageError("config", `${name}'s inputSchema names one of session_key, harness and conversation_id, which Wakeshift adds itself: rename it`);
    }
    if (propsOf(config.outputSchema).includes(WAKESHIFT_FIELD)) {
      throw new UsageError("config", `${name}'s outputSchema names wakeshift, which Wakeshift adds itself: rename it`);
    }
    const server = this.server as { registerTool?: (...args: unknown[]) => unknown };
    if (typeof server?.registerTool !== "function") {
      throw new UsageError("config", "registerTool needs an McpServer: on a low-level Server, name the tool in attach's wrap and subscribe with wake.currentCall()");
    }
    const h = handler;
    const layer = this.layer;
    const wake = this.wake;
    // The MCP SDK calls the tool's callback as (args, ctx), or (ctx) without
    // an inputSchema; the handler gets the same with call appended.
    const cb = async (...args: unknown[]): Promise<CallToolResult> => {
      const ctx = args[args.length - 1];
      let call: unknown;
      if (!layer) call = new DisabledCall(wake, name, ctx, events);
      else {
        const scope = findCall(wake, ctx);
        // Without the tools/call layer (an SDK whose handlers Wakeshift can't
        // reach) the call still works, without a block.
        call = scope && scope.tool === name ? scope : await layer.begin(name, {}, ctx, events);
      }
      const result = await h(...args, call);
      if (typeof result === "string") {
        if (call instanceof CallScope) call.stringResult = true;
        return { content: [{ type: "text", text: result }] };
      }
      return result;
    };
    layer?.declare(name, events);
    return server.registerTool(name, hostConfig, cb);
  }
}

/** @internal Checks a tool's subscribes: a list of this client's events. */
export function checkSubscribes(wake: Wakeshift, tool: string, list: unknown): readonly AnyEvent[] {
  if (!Array.isArray(list)) throw new UsageError("config", `${tool}: subscribes lists the events the tool subscribes to ([] for none)`);
  for (const e of list) {
    if (!e || typeof (e as AnyEvent).owner !== "function" || (e as AnyEvent).owner() !== wake) {
      throw new UsageError("config", `${tool}: subscribes lists ${(e as { name?: unknown })?.name ?? String(e)}, which is another client's event or not an event (wake.defineEvent)`);
    }
  }
  return [...list];
}
