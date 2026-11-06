// Events: what an app offers to be subscribed to, the way MCP Events
// (SEP-3415) lists them. An app defines its event types (defineEvent),
// serves them (attach) and emits occurrences (emit). Wakeshift
// delivers each occurrence to every subscriber the best way its client
// allows: natively (MCP Events, a webhook) for a client that speaks the
// extension, and through Wakeshift's wake paths for everyone else, who
// subscribe with the subscribe_events tool attach adds.
// https://wakeshift.ai/docs/events has the guide.

import { RpcError, type AuthInfo, type CallToolResult, type LowServerLike, type McpServerLike, type ToolCallExtra } from "./mcp.js";
import { isSchema, isZ4Schema, objectFromShape, safeParse, toJsonSchema, type AnySchema, type SchemaInput, type SchemaOutput, type ZodRawShapeCompat } from "./zod.js";
import { capabilitiesError, hostOf, type Host } from "./host.js";
import { APIConnectionError, APIError, UsageError, detailOf, errorType, type Issue } from "./errors.js";
import {
  DELIVERY,
  EXTENSION_KEY,
  HEADER_MISMATCH,
  INTERNAL_ERROR,
  INVALID_PARAMS,
  TOP_LEVEL_KEY,
  NAME_HEADER,
  RESULT_TYPE,
  bridgedClient,
  codesFor,
  dialectOf,
  headerValue,
  type Codes,
  type Dialect,
} from "./spec.js";
import { agentToken, publishUrlOf, toolCallOf, transportSessionOf, userAgentOf, type Agent, type Emitted, type PublishUrl, type Recipient, type Via, type Wakeshift } from "./wakeshift.js";
import { Attachment, ToolLayer, checkSubscribes, loadCallStore, nativeOf, wakeInputsFor, type AnyEvent } from "./toolcall.js";
import { serveStateless } from "./discover.js";
import { COMPACT, OUTPUT_SCHEMAS } from "./wording.js";
import { optionalModules } from "./optional.js";
import { serveTasks, TOOL_ANNOTATIONS } from "./tasks.js";
import { takeVoucherContext } from "./voucherctx.js";
import { runOnSubscribe, startWatch, type Subscribed, type Watch, type WatchCheck, type WatchOptions } from "./watch.js";

/**
 * Who is subscribing: what authorize decides on. Never decide on client or
 * native: both are client-asserted. authInfo, headers and ctx don't print
 * (they're not enumerable).
 */
export interface Subscriber {
  /** The app's ID for the person (attach's identify), if it has one. */
  user?: string;
  /** The MCP client's name, if known. Client-asserted. */
  client: string;
  /** Subscribing natively (MCP Events), or through subscribe_events. Client-asserted. */
  native: boolean;
  /** The caller's verified token, as your MCP SDK's auth middleware passed it (the same shape on 1.x and 2.x). */
  readonly authInfo?: AuthInfo;
  /** The request's HTTP headers, names in lowercase. Not verified. */
  readonly headers?: Record<string, string>;
  /** The request's context, as your MCP SDK passes it to handlers. */
  readonly ctx?: unknown;
}

/** @internal How long identify and authorize may take before they refuse (tests shorten them). */
export const limits = { identifyMs: 10_000, authorizeMs: 10_000 };

/** @internal A promise that rejects with a TimeoutError after ms. */
export function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(Object.assign(new Error("took too long"), { name: "TimeoutError" })), ms);
    (timer as { unref?: () => void }).unref?.();
  });
  return Promise.race([p, late]).finally(() => clearTimeout(timer));
}

/** @internal A Subscriber, with the caller's credential and context out of every printer. */
export function subscriberOf(user: string | undefined, client: string, native: boolean, req: { authInfo?: AuthInfo; headers?(): Record<string, string> } | undefined, ctx: unknown): Subscriber {
  const s: Subscriber = { user, client, native };
  const shown = () => ({ user, client, native, authInfo: req?.authInfo ? "[redacted]" : undefined });
  Object.defineProperties(s, {
    authInfo: { value: req?.authInfo, enumerable: false },
    headers: { value: req?.headers?.() ?? {}, enumerable: false },
    ctx: { value: ctx, enumerable: false },
    toJSON: { value: shown, enumerable: false },
    [Symbol.for("nodejs.util.inspect.custom")]: { value: shown, enumerable: false },
  });
  return s;
}

export interface EventOptions<A extends AnySchema, P extends AnySchema, Personal extends boolean = boolean> {
  /** What it is, for the agent and the person: "A new message in a channel". */
  description: string;
  /** A short title, for clients that show one. */
  title?: string;
  /**
   * What a subscriber picks: a zod object (or a zod shape). Its required
   * string, number and boolean fields route occurrences (each value is a
   * topic segment: lowercase a-z 0-9 . _ -); an optional one filters on the
   * payload field of the same name.
   */
  args: A | ZodRawShapeCompat;
  /** What each occurrence carries: a zod object. */
  payload: P;
  /**
   * The line an agent reads first for an occurrence: `${from}: ${text}`.
   * Optional: without it the agent reads the description and the
   * arguments ("A crawl finished (job 3f2a)").
   */
  text?: (payload: SchemaOutput<P>, args: SchemaOutput<A>) => string;
  /**
   * Opens the event to subscribe_events and native MCP Events
   * subscriptions, gated by this: whether subscriber may subscribe with
   * these arguments. Without personal, authorize or public, only the app's
   * own tools can subscribe to the event (the default, and what the core
   * enforces). Never decide on client or native: both are client-asserted.
   */
  authorize?: (subscriber: Subscriber, args: SchemaOutput<A>) => boolean | Promise<boolean>;
  /**
   * Anyone may subscribe, through subscribe_events or natively (a native
   * client still needs a signed-in user): for truly public streams. Can't
   * be combined with personal or authorize.
   */
  public?: boolean;
  /**
   * Each subscription belongs to its subscriber (attach's identify), and
   * emit sends an occurrence to one of them (`to`), so it may carry that
   * subscriber's own private view. Nothing in args names the subscriber:
   * Wakeshift takes it from the subscriber's own sign-in.
   */
  personal?: Personal;
  /**
   * Wakeshift deletes an occurrence's content (its text and payload) once
   * it's delivered to the agent, keeping only what shows it happened (IDs,
   * times, where it went): for messages people wouldn't expect a third party
   * to keep. One that isn't delivered expires as any other does.
   */
  deleteOnDelivery?: boolean;
  /**
   * Advanced: the topic template and payload filter, if the defaults don't
   * fit. A filter without a topic adds to the optional arguments' filters,
   * and may not name the same payload field as one of them.
   */
  topic?: string;
  filter?: Record<string, string>;
  /**
   * Runs once for each new subscription to the event, whichever path made
   * it: one of the app's tools (after its call succeeded), subscribe_events,
   * or an MCP Events client's events/subscribe or events/poll. In the
   * background: the request doesn't wait for it, and a throw is logged
   * (on_subscribe_failed) while the subscription stands. For a vendor with
   * no webhook, start the watch here, and every path is covered:
   *
   *   onSubscribe: ({ args }) => { crawlFinished.watch(args, check) }
   *
   * Annotate its parameter with your context's type to read s.context
   * without a cast: `onSubscribe: (s: Subscribed<{ job: string }, ApiKey>) => …`.
   */
  // A method, so a handler annotated with Subscribed<Args, YourContext> type-checks.
  onSubscribe?(s: Subscribed<SchemaOutput<A>>): void | Promise<void>;
}

/**
 * How one request to Wakeshift goes, the last parameter of every async
 * method: signal ends it early (its reason is rethrown, unchanged), timeoutMs
 * replaces the client's timeout, maxRetries the client's retries.
 */
export interface RequestOptions {
  signal?: AbortSignal;
  timeoutMs?: number;
  maxRetries?: number;
}

/** A personal event's recipients: one or a list, mixed (at most 1,000). */
export type Recipients = string | Recipient | (string | Recipient)[];

export interface EmitOptions extends RequestOptions {
  /**
   * Makes retries emit once for a day (at most 200 characters): a retry of
   * the same emit (arguments, recipients and payload) returns the first's
   * result with `duplicate: true`. The id on a different emit throws
   * IdReusedError (`id_reused`) and sends nothing: give each occurrence its own.
   */
  id?: string;
  /**
   * A personal event's recipients (at most 1,000), one or a list, mixed: a
   * user, the app's ID for them (attach's identify), reaching every
   * subscription they have to the event with these arguments;
   * `{ user, subscription }`, exactly that subscription of that user's;
   * `{ native: user }`, only that user's native (webhook) subscriptions.
   * Required for personal events, refused otherwise. Take it from your own
   * record of who owns what, never from the subscriber list.
   */
  to?: Recipients;
}

// What a personal event's methods take last: a personal event needs to
// (required by the type), a shared one takes none, and one whose personal
// isn't a literal is checked at run time.
// boolean extends Personal first: true for boolean and for any (AnyEvent),
// both checked at run time.
type ToOptions<Personal, O, To> = boolean extends Personal
  ? [opts?: O & { to?: To }]
  : [Personal] extends [true]
    ? [opts: O & { to: To }]
    : [opts?: O & { to?: never }];

/** The manifest form of an event type. */
export interface EventTypeManifest {
  name: string;
  description: string;
  arguments: Record<string, unknown>;
  payload: Record<string, unknown>;
  topic: string;
  filter?: Record<string, string>;
  personal?: boolean;
  delete_on_delivery?: boolean;
  /** Who may subscribe: "tools" (only the app's tools) or "any" (also subscribe_events and native clients, as the event allows). The core keeps the last value it was sent. */
  subscribe?: "tools" | "any";
}

/** @internal The most bytes an event's text takes (UTF-8, JSON-escaped) before monitor_line_long. */
export const MONITOR_TEXT_BUDGET = 200;

/** @internal A text's length as the Monitor line carries it: UTF-8 bytes, JSON-escaped, without the quotes. */
export function textBytes(text: string): number {
  return new TextEncoder().encode(JSON.stringify(text)).length - 2;
}

/** @internal The event argument a native client passes a subscribe voucher in (docs/subscribe-vouchers.md §4.4): reserved, and taken out before anything reads the arguments. */
export const VOUCHER_ARG = "wakeshift_voucher";

/**
 * @internal arguments without wakeshift_voucher, and the voucher: a
 * non-empty string, else undefined (anything else in its place is no
 * voucher). Only the exact name: Wakeshift_Voucher and the like stay
 * arguments, and fail as any unknown argument does.
 */
export function takeVoucher(args: unknown): { args: unknown; voucher: string | undefined } {
  if (!args || typeof args !== "object" || Array.isArray(args) || !Object.prototype.hasOwnProperty.call(args, VOUCHER_ARG)) return { args, voucher: undefined };
  const { [VOUCHER_ARG]: v, ...rest } = args as Record<string, unknown>;
  return { args: rest, voucher: typeof v === "string" && v ? v : undefined };
}

const nameRE = /^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*){0,4}$/;
const argNameRE = /^[a-z][a-z0-9_]{0,63}$/;

// jsonSchemaOf is a zod schema as JSON Schema, without $schema (Wakeshift
// reads it as 2020-12). loose: an object that doesn't refuse unknown keys
// (zod 3's default, strip) says nothing of additional properties, as zod 4
// renders it, rather than zod-to-json-schema's additionalProperties: false.
function jsonSchemaOf(s: AnySchema, loose = false): Record<string, any> {
  const out = toJsonSchema(s) as Record<string, any>;
  delete out.$schema;
  if (loose && !isZ4Schema(s)) loosen(out, s);
  normalize(out);
  return out;
}

// loosen drops additionalProperties: false from each object in a zod 3
// schema's JSON Schema whose zod object isn't .strict(), walking the two
// side by side. A part it can't match up (a $ref) is left as it was.
function loosen(json: unknown, schema: unknown): void {
  if (!json || typeof json !== "object" || Array.isArray(json) || !schema) return;
  const j = json as Record<string, any>;
  if (typeof j.$ref === "string") return;
  const def = (schema as { _def?: Record<string, any> })._def;
  if (!def) return;
  switch (def.typeName) {
    case "ZodObject": {
      if (def.unknownKeys !== "strict" && j.additionalProperties === false) delete j.additionalProperties;
      const shape = typeof def.shape === "function" ? def.shape() : def.shape;
      if (shape && j.properties && typeof j.properties === "object") for (const k of Object.keys(shape)) loosen(j.properties[k], shape[k]);
      if (def.catchall && j.additionalProperties && typeof j.additionalProperties === "object") loosen(j.additionalProperties, def.catchall);
      return;
    }
    case "ZodOptional":
    case "ZodDefault":
    case "ZodCatch":
    case "ZodReadonly":
      return loosen(json, def.innerType);
    case "ZodBranded":
      return loosen(json, def.type);
    case "ZodEffects":
      return loosen(json, def.schema);
    case "ZodPipeline":
      return loosen(json, def.in);
    case "ZodLazy":
      return loosen(json, def.getter());
    case "ZodNullable":
      // { anyOf: [inner, { type: "null" }] }, or the inner schema with null in its type.
      if (Array.isArray(j.anyOf)) for (const b of j.anyOf) loosen(b, def.innerType);
      else loosen(json, def.innerType);
      return;
    case "ZodArray":
      return loosen(j.items, def.type);
    case "ZodRecord":
      return loosen(j.additionalProperties, def.valueType);
    case "ZodUnion":
    case "ZodDiscriminatedUnion": {
      const options = Array.isArray(def.options) ? def.options : [...(def.options?.values?.() ?? [])];
      const branches = j.anyOf ?? j.oneOf;
      if (Array.isArray(branches) && branches.length === options.length) options.forEach((o: unknown, i: number) => loosen(branches[i], o));
      return;
    }
    case "ZodIntersection":
      if (Array.isArray(j.allOf) && j.allOf.length === 2) {
        loosen(j.allOf[0], def.left);
        loosen(j.allOf[1], def.right);
      }
      return;
  }
}

// normalize puts a schema in the form all three SDKs give for the same
// definition (test/events-list.json): required in name order, whatever
// order the fields were defined in, and an integer without the bounds zod
// adds for .int() (JavaScript's safe integers), which the other SDKs don't.
function normalize(s: Record<string, any>): void {
  if (Array.isArray(s.required)) s.required = [...s.required].sort();
  if (s.type === "integer") {
    if (s.minimum === Number.MIN_SAFE_INTEGER) delete s.minimum;
    if (s.maximum === Number.MAX_SAFE_INTEGER) delete s.maximum;
  }
  if (s.properties && typeof s.properties === "object") {
    for (const p of Object.values(s.properties)) if (p && typeof p === "object") normalize(p as Record<string, any>);
  }
  if (s.items && typeof s.items === "object" && !Array.isArray(s.items)) normalize(s.items);
  for (const k of ["anyOf", "oneOf", "allOf"]) {
    if (Array.isArray(s[k])) for (const b of s[k]) if (b && typeof b === "object") normalize(b);
  }
}

// canonical is JSON with every object's keys in name order, as Wakeshift
// keeps a subscription's arguments: how the tools' results echo them.
function canonical(v: unknown): string {
  const sortKeys = (x: any): any =>
    Array.isArray(x) ? x.map(sortKeys) : x && typeof x === "object" ? Object.fromEntries(Object.keys(x).sort().map((k) => [k, sortKeys(x[k])])) : x;
  return JSON.stringify(sortKeys(v) ?? {});
}

// issuesOf is a schema's refusal as the SDK's issues: each problem and where.
function issuesOf(err: unknown): Issue[] {
  const list = (err as { issues?: { path?: PropertyKey[]; message?: string }[] })?.issues;
  if (!Array.isArray(list)) return [];
  return list.map((i) => ({ message: String(i.message ?? ""), path: (i.path ?? []).map((p) => (typeof p === "number" ? p : String(p))) }));
}

// argsError is the UsageError for arguments or a payload a schema refused.
function argsError(what: string, err: unknown): UsageError {
  const issues = issuesOf(err);
  const said = issues.map((i) => (i.path.length ? i.path.join(".") + ": " : "") + i.message).join("; ");
  return new UsageError("bad_arguments", said ? `${what}: ${said}` : what, { issues });
}

/** One of the app's event types (wake.defineEvent). N is its name, as defineEvent was given it. */
/** Who may subscribe to an event: only the app's tools (the default), or also others, gated by personal, authorize or public. */
export type Access = "tools" | "personal" | "authorize" | "public";

const EVENT_OPTIONS = new Set(["description", "title", "args", "payload", "text", "authorize", "public", "personal", "deleteOnDelivery", "topic", "filter", "onSubscribe"]);

/**
 * One of the app's event types (wake.defineEvent): N its name, A its
 * arguments' schema, P its payload's, Personal whether it's personal.
 */
export class WakeEvent<N extends string = string, A extends AnySchema = AnySchema, P extends AnySchema = AnySchema, Personal extends boolean = boolean> {
  readonly name: N;
  /** @internal */ readonly description: string;
  /** @internal */ readonly title?: string;
  /** @internal */ readonly personal: boolean;
  /** @internal */ readonly deleteOnDelivery: boolean;
  /** @internal Who may subscribe: only the app's tools, or also others, by personal, authorize or public. */
  readonly access: Access;
  /** @internal */ readonly argsSchema: AnySchema;
  /** @internal */ readonly payloadSchema: AnySchema;
  /** @internal */ readonly manifest: EventTypeManifest;
  private readonly opts: EventOptions<A, P, Personal>;

  /** @internal Use wake.defineEvent. */
  constructor(
    private readonly wake: Wakeshift,
    name: N,
    opts: EventOptions<A, P, Personal>,
  ) {
    if (!nameRE.test(name) || name.length > 64) throw new UsageError("config", `event name ${JSON.stringify(name)}: lowercase dot-separated words (chat.message), at most 64 characters`);
    if (name.startsWith("wakeshift.")) throw new UsageError("config", "event names starting with wakeshift. are Wakeshift's");
    if (!opts || typeof opts !== "object") throw new UsageError("config", `event ${name} needs its options: description, args and payload`);
    for (const k of Object.keys(opts)) {
      if (!EVENT_OPTIONS.has(k)) throw new UsageError("config", `event ${name} has no option ${k}: its options are ${[...EVENT_OPTIONS].join(", ")}`);
    }
    if (typeof opts.description !== "string" || !opts.description.trim()) throw new UsageError("config", `event ${name} needs a description`);
    if (opts.text !== undefined && typeof opts.text !== "function") throw new UsageError("config", `event ${name}: text is a function, (payload, args) => string`);
    if (opts.authorize !== undefined && typeof opts.authorize !== "function") throw new UsageError("config", `event ${name}: authorize is a function, (subscriber, args) => boolean`);
    if (opts.onSubscribe !== undefined && typeof opts.onSubscribe !== "function") throw new UsageError("config", `event ${name}: onSubscribe is a function, ({ args, user, via, tool }) => void`);
    for (const k of ["personal", "public", "deleteOnDelivery"] as const) {
      if (opts[k] !== undefined && typeof opts[k] !== "boolean") throw new UsageError("config", `event ${name}: ${k} is true or false`);
    }
    if (opts.public && (opts.personal || opts.authorize)) {
      throw new UsageError("config", `event ${name}: public can't be combined with personal or authorize (public opens it to anyone; personal and authorize gate it)`);
    }
    this.name = name;
    this.description = opts.description;
    this.title = opts.title;
    this.personal = !!opts.personal;
    this.deleteOnDelivery = !!opts.deleteOnDelivery;
    this.access = opts.public ? "public" : opts.personal ? "personal" : opts.authorize ? "authorize" : "tools";
    this.opts = opts;
    this.argsSchema = isSchema(opts.args) ? opts.args : (objectFromShape((opts.args ?? {}) as ZodRawShapeCompat) as AnySchema);
    this.payloadSchema = opts.payload;
    const args = jsonSchemaOf(this.argsSchema);
    // A payload a sender may add fields to (a webhook's body) mustn't refuse them.
    const payload = jsonSchemaOf(this.payloadSchema, true);
    if (args.type !== "object") throw new UsageError("config", `event ${name}: args is a zod object`);
    if (payload.type !== "object") throw new UsageError("config", `event ${name}: payload is a zod object`);
    const props = (args.properties ?? {}) as Record<string, { type?: string }>;
    // A native client writes a subscribe voucher there (docs/subscribe-vouchers.md §4.4).
    if (Object.prototype.hasOwnProperty.call(props, VOUCHER_ARG)) throw new UsageError("config", `event ${name}: ${VOUCHER_ARG} is Wakeshift's: a native client passes a subscribe voucher in it. Name the argument something else`);
    const required = new Set<string>(args.required ?? []);
    const payloadProps = (payload.properties ?? {}) as Record<string, unknown>;
    let topic = opts.topic;
    let filter = opts.filter;
    if (topic === undefined) {
      // ev/<name>/{a}/{b}: one segment per required scalar argument; an
      // optional one filters on the payload field of its name.
      const segs = ["ev", name];
      filter = { ...filter }; // the caller's object stays as it was
      for (const arg of Object.keys(props).sort()) {
        if (!argNameRE.test(arg)) throw new UsageError("config", `event ${name}: argument ${JSON.stringify(arg)}: lowercase letters, digits and _`);
        const t = props[arg]?.type;
        const scalar = t === "string" || t === "integer" || t === "number" || t === "boolean";
        if (required.has(arg)) {
          if (!scalar || t === "number") throw new UsageError("config", `event ${name}: argument ${arg} routes occurrences, so it's a string, integer or boolean (or pass topic)`);
          segs.push(`{${arg}}`);
        } else if (scalar && arg in payloadProps) {
          // Both on the same field is refused: neither can quietly win.
          if (opts.filter && Object.prototype.hasOwnProperty.call(opts.filter, arg)) throw new UsageError("config", `event ${name}: filter on ${arg} conflicts with optional argument ${arg}, which already filters that payload field (remove it from filter, or pass topic and filter)`);
          filter[arg] = `{${arg}}`;
        } else {
          throw new UsageError("config", `event ${name}: optional argument ${arg} filters on the payload field of its name, which the payload doesn't have (or pass topic and filter)`);
        }
      }
      topic = segs.join("/");
      if (Object.keys(filter).length === 0) filter = undefined;
    }
    this.manifest = {
      name,
      description: opts.description,
      arguments: args,
      payload,
      topic,
      ...(filter ? { filter } : {}),
      ...(this.personal ? { personal: true } : {}),
      ...(this.deleteOnDelivery ? { delete_on_delivery: true } : {}),
      // Who may subscribe, which the core enforces too: "tools" unless the
      // event says otherwise (design §3).
      subscribe: this.open ? "any" : "tools",
    };
  }

  toJSON(): { name: N } {
    return { name: this.name };
  }

  [Symbol.for("nodejs.util.inspect.custom")](): string {
    return `WakeEvent { name: ${JSON.stringify(this.name)} }`;
  }

  /** @internal Whether others than the app's tools may subscribe: subscribe_events and native clients. */
  get open(): boolean {
    return this.access !== "tools";
  }

  /** @internal Whether the event has an authorize. */
  get gated(): boolean {
    return typeof this.opts.authorize === "function";
  }

  /** @internal The line the agent reads: the event's text, else its description and arguments. */
  render(payload: unknown, args: Record<string, unknown>): string {
    if (this.opts.text) return this.opts.text(payload as SchemaOutput<P>, args as SchemaOutput<A>);
    // The same in every SDK: the description (whitespace collapsed, trailing
    // ". " trimmed), then the arguments sorted, "key value", skipping null
    // and empty strings; strings raw, other values compact JSON.
    const d = this.description.replace(/\s+/g, " ").trim().replace(/[.\s]+$/, "");
    const parts = Object.keys(args ?? {})
      .sort()
      .filter((k) => args[k] !== undefined && args[k] !== null && args[k] !== "")
      .map((k) => `${k} ${typeof args[k] === "string" ? args[k] : JSON.stringify(args[k])}`);
    return parts.length ? `${d} (${parts.join(", ")})` : d;
  }

  /** @internal Checks a subscriber's arguments, returning them parsed. */
  parseArgs(args: unknown): SchemaOutput<A> {
    const r = safeParse(this.argsSchema, args ?? {});
    if (!r.success) throw argsError(`arguments for ${this.name}`, r.error);
    return r.data as SchemaOutput<A>;
  }

  /** @internal Whether this subscriber may subscribe with these arguments. */
  async allows(subscriber: Subscriber, args: SchemaOutput<A>): Promise<boolean> {
    if (!this.opts.authorize) return true;
    try {
      return (await withTimeout(Promise.resolve().then(() => this.opts.authorize!(subscriber, args)), limits.authorizeMs)) === true;
    } catch (err) {
      // An authorize that fails or hangs refuses, the same way on every
      // path; the app hears its error's type, never its message.
      this.wake.log.log("authorize_failed", `authorize for ${this.name} threw, failed or took too long, so the subscription was refused`, { event: this.name, error_type: errorType(err) });
      return false;
    }
  }

  /**
   * @internal The events/list entry. With voucherApp (a tools-only event
   * listed to a native request while the core hands out subscribe
   * vouchers), its arguments gain the required wakeshift_voucher, described
   * with the app's name.
   */
  definition(voucherApp?: string): Record<string, unknown> {
    let inputSchema = this.manifest.arguments;
    if (voucherApp !== undefined) {
      const properties = { ...(inputSchema.properties ?? {}), [VOUCHER_ARG]: { type: "string", description: fill("voucher_argument", { app: voucherApp || UNNAMED_APP }) } };
      inputSchema = { ...inputSchema, properties, required: [...((inputSchema.required as string[] | undefined) ?? []), VOUCHER_ARG].sort() };
    }
    const d: Record<string, unknown> = {
      name: this.name,
      description: this.description,
      // A tools-only event is never polled (events/poll refuses it, voucher
      // or not): only a webhook subscription with a voucher reaches it.
      delivery: this.open ? [...DELIVERY] : ["webhook"],
      inputSchema,
      payloadSchema: this.manifest.payload,
    };
    if (this.title) d.title = this.title;
    return d;
  }

  /** @internal The client the event was defined on. */
  owner(): Wakeshift {
    return this.wake;
  }

  /** @internal Runs onSubscribe for a new subscription, in the background. */
  subscribed(s: Subscribed<any>, runIn?: <R>(fn: () => R) => R): void {
    runOnSubscribe(this.wake, this as unknown as WakeEvent, this.opts.onSubscribe as ((s: Subscribed<any>) => unknown) | undefined, s, runIn);
  }

  /**
   * For work your API offers no usable webhook for: calls check until it
   * returns the payload (undefined: keep waiting), then emits it once with
   * these arguments. First check at once, then every `every` (default 30
   * seconds; after errors in a row, longer, up to 5 minutes), until
   * `timeout` (default 1 hour). Returns at once: watch.done resolves (never
   * rejects) with how it ended. A watch of the same arguments while one
   * runs returns that one. At most 10 checks run at once per client.
   * It runs in this process only: a restart, or a serverless function
   * freezing, ends it (on Cloudflare Workers, use a Durable Object alarm).
   * wake.close() stops every watch. A disabled client runs nothing.
   *
   *   crawlFinished.watch({ job }, async ({ signal }) => {
   *     const s = await api.status(job, { signal })
   *     return s.done ? { status: s.state, pages: s.pages } : undefined
   *   }, { every: "10s", timeout: "2h" })
   */
  watch(args: SchemaInput<A>, check: WatchCheck<SchemaInput<P>>, ...rest: ToOptions<Personal, WatchOptions, Recipients>): Watch {
    return startWatch(this.wake, this as unknown as WakeEvent, args, check, rest[0] as WatchOptions & { to?: unknown });
  }

  /**
   * Sends one occurrence: to everyone subscribed with these arguments, or,
   * for a personal event, to the recipients `to` names. The payload is
   * checked against the event's schema first.
   */
  async emit(args: SchemaInput<A>, payload: SchemaInput<P>, ...rest: ToOptions<Personal, Omit<EmitOptions, "to">, Recipients>): Promise<Emitted> {
    const opts: EmitOptions = rest[0] ?? {};
    const a = this.parseArgs(args);
    const p = safeParse(this.payloadSchema, payload);
    if (!p.success) throw argsError(`payload for ${this.name}`, p.error);
    const to = this.wireTo(opts.to);
    if (this.personal && !to) throw new UsageError("bad_arguments", `${this.name} is personal: pass { to }, the subscribers it's for`);
    if (!this.personal && to) throw new UsageError("bad_arguments", `${this.name} isn't personal: it goes to everyone subscribed with these arguments, so it takes no to`);
    // A disabled client checks, and sends nothing.
    if (!this.wake.enabled) return { matched: 0, duplicate: false, unmatched: [], ended: [], textTrimmed: false };
    const text = this.render(p.data, a as Record<string, unknown>);
    // Claude Code's Monitor shows a 480-byte line, about 230 of which are
    // Wakeshift's own fields: past 200 bytes, the text leaves the payload no room.
    // Text over 1000 bytes is trimmed by Wakeshift, which text_trimmed reports instead.
    if (typeof text === "string" && new TextEncoder().encode(text).length <= 1000 && textBytes(text) > MONITOR_TEXT_BUDGET) {
      this.wake.log.log("monitor_line_long", `${this.name}'s text is ${textBytes(text)} bytes, so on Claude Code's Monitor line it leaves no room for the payload`, { event: this.name });
    }
    return this.wake.emitEvent({ name: this.name, arguments: a, payload: p.data, text, id: opts.id, to }, opts);
  }

  // wireTo is what the core takes for to: one plain user as a string,
  // anything else as a list of "user", { user, subscription } and
  // { native: user }, each name trimmed of the whitespace around it, as every
  // SDK does (test/emit-recipients.json). undefined: no recipients.
  private wireTo(to: EmitOptions["to"]): string | (string | Recipient)[] | undefined {
    if (to === undefined) return undefined;
    const empty = (what: string) => new UsageError("bad_arguments", `${this.name}: a recipient's ${what} is empty`);
    const out: (string | Recipient)[] = [];
    for (const r of Array.isArray(to) ? to : [to]) {
      if (typeof r === "string") {
        if (!r.trim()) throw empty("user");
        out.push(r.trim());
      } else if (r && typeof r === "object" && "native" in r && typeof r.native === "string") {
        if (!r.native.trim()) throw empty("user");
        out.push({ native: r.native.trim() });
      } else if (r && typeof r === "object" && "user" in r && typeof r.user === "string" && typeof r.subscription === "string") {
        if (!r.user.trim()) throw empty("user");
        if (!r.subscription.trim()) throw empty("subscription");
        out.push({ user: r.user.trim(), subscription: r.subscription.trim() });
      } else {
        throw new UsageError("bad_arguments", `${this.name}: a recipient is a user, { user, subscription } or { native: user }`);
      }
    }
    if (out.length === 0) return undefined;
    if (out.length === 1 && typeof out[0] === "string") return out[0];
    return out;
  }

  /**
   * A capability URL that emits this event with these arguments (a personal
   * event: to the one subscriber `to` names), for services that can only
   * send webhooks. A POST's body, a JSON object, is the payload, checked
   * against the event's schema; since `text` doesn't run, the agent reads
   * the event's default text and the payload whole. Anyone with the URL can
   * emit until `wake.revokePublishUrl(id)`, unless it has a signingSecret: the
   * sender's Standard Webhooks secret (whsec_…, a Svix endpoint's), and then
   * the URL takes only posts signed with it. The secret is sent to Wakeshift
   * once and never returned: the result's signing has its fingerprint.
   */
  async createPublishUrl(args: SchemaInput<A>, ...rest: ToOptions<Personal, Omit<PublishUrlOptions, "to">, string>): Promise<PublishUrl> {
    const opts: PublishUrlOptions = rest[0] ?? {};
    const a = this.parseArgs(args);
    if (opts.to !== undefined && typeof opts.to !== "string") {
      throw new UsageError("bad_arguments", `${this.name}: a publish URL's to is exactly one user`);
    }
    const to = opts.to?.trim();
    if (this.personal && !to) throw new UsageError("bad_arguments", `${this.name} is personal: pass { to }, the one subscriber it's for`);
    if (!this.personal && to) throw new UsageError("bad_arguments", `${this.name} isn't personal: it goes to everyone subscribed with these arguments, so it takes no to`);
    if (opts.signingSecret !== undefined && (typeof opts.signingSecret !== "string" || !opts.signingSecret.trim())) {
      throw new UsageError("bad_signing_secret", `${this.name}: a publish URL's signingSecret is a Standard Webhooks secret (whsec_…)`);
    }
    const body: Record<string, unknown> = { name: this.name, arguments: a };
    if (to) body.to = to;
    if (opts.signingSecret !== undefined) body.signing_secret = opts.signingSecret.trim();
    const out = await this.wake.requestEvent<{ id: string; event?: string; url: string; signing?: { fingerprint: string; set_at: string; previous_valid_until?: string } }>(
      "/events/publish-urls",
      body,
      opts,
    );
    const signing = out.signing ? { fingerprint: out.signing.fingerprint, setAt: out.signing.set_at, ...(out.signing.previous_valid_until ? { previousValidUntil: out.signing.previous_valid_until } : {}) } : undefined;
    return publishUrlOf(out.id, out.url, out.event, signing);
  }

  /**
   * How many conversations are subscribed to this event with these
   * arguments (a personal event: those of the one user `to` names), how
   * many are connected now, and how. Only reads.
   */
  async status(args: SchemaInput<A>, ...rest: ToOptions<Personal, RequestOptions, string>): Promise<EventStatus> {
    const opts: RequestOptions & { to?: string } = rest[0] ?? {};
    const a = this.parseArgs(args);
    if (opts.to !== undefined && typeof opts.to !== "string") {
      throw new UsageError("bad_arguments", `${this.name}: status's to is exactly one user`);
    }
    const to = opts.to?.trim();
    if (this.personal && !to) throw new UsageError("bad_arguments", `${this.name} is personal: pass { to }, the one user to count for`);
    if (!this.personal && to) throw new UsageError("bad_arguments", `${this.name} isn't personal: status takes no to`);
    const body: Record<string, unknown> = { name: this.name, arguments: a };
    if (to) body.to = to;
    const out = await this.wake.requestEvent<{ subscriptions: number; connected: number; via?: Partial<Record<Via, number>> }>("/events/status", body, opts);
    return { subscriptions: out.subscriptions, connected: out.connected, via: { stream: 0, poll: 0, hooks: 0, webhook: 0, ...out.via } };
  }

  /**
   * Reads subscriptions to this event by ID, each with the user
   * `call.subscribe` made it for, up to 1,000 at once: whether each is live
   * and its conversation connected now, and how. Only reads.
   */
  async statusOf(subs: { user: string; subscription: string }[], opts: RequestOptions = {}): Promise<SubscriptionStatus[]> {
    if (subs.length === 0) return [];
    if (subs.length > MAX_STATUS_SUBSCRIPTIONS) {
      throw new UsageError("bad_request", `${this.name}: at most ${MAX_STATUS_SUBSCRIPTIONS} subscriptions in one statusOf`);
    }
    const refs = subs.map((s) => {
      if (!s || typeof s.user !== "string" || !s.user.trim() || typeof s.subscription !== "string" || !s.subscription.trim()) {
        throw new UsageError("bad_request", `${this.name}: statusOf needs each subscription's user and subscription`);
      }
      return { user: s.user.trim(), id: s.subscription.trim() };
    });
    const out = await this.wake.requestEvent<{ subscriptions?: { id: string; user: string; live: boolean; connected: boolean; via?: Via }[] }>(
      "/events/status",
      { name: this.name, subscriptions: refs },
      opts,
    );
    return (out.subscriptions ?? []).map((s) => {
      const r: SubscriptionStatus = { id: s.id, user: s.user, live: !!s.live, connected: !!s.connected };
      if (s.via) r.via = s.via;
      return r;
    });
  }

  /**
   * Ends one of user's subscriptions to this app's events, by its ID (a
   * Subscription's id from call.subscribe, or list_subscriptions'), outside
   * a tool: a player who timed out, a member removed. False if there was none.
   */
  async end(user: string, id: string, opts: RequestOptions = {}): Promise<boolean> {
    const out = await this.wake.requestApp<{ ended: boolean }>("POST", "/event-subscriptions/end", { user, id }, opts);
    return !!out.ended;
  }
}

/** How event.createPublishUrl makes a URL. */
export interface PublishUrlOptions extends RequestOptions {
  /** For a personal event: the one user its posts are for. */
  to?: string;
  /**
   * The sender's Standard Webhooks signing secret (whsec_ and the base64 of
   * 24 to 64 bytes, a Svix endpoint's): the URL then takes only posts signed
   * with it. Keep it out of logs; Wakeshift never returns it.
   */
  signingSecret?: string;
}

/** The most subscriptions one `statusOf` reads. */
export const MAX_STATUS_SUBSCRIPTIONS = 1000;

/** Who is subscribed to an event with some arguments right now (`event.status`). */
export interface EventStatus {
  /** Conversations subscribed (ended or expired subscriptions don't count). */
  subscriptions: number;
  /** Of those, connected now. */
  connected: number;
  /** How many are connected each way. */
  via: Record<Via, number>;
}

/** One subscription right now (`event.statusOf`). */
export interface SubscriptionStatus {
  id: string;
  user: string;
  /** The subscription is the user's, to this event, and hasn't ended. One that names nothing is false too. */
  live: boolean;
  /** Its conversation is reachable now, and `via` says how. */
  connected: boolean;
  via?: Via;
}

/** Who is behind a request to the app's MCP server, as the app knows them (AttachOptions.identify). */
export interface Identity {
  /** The app's ID for the person: what personal events and list_subscriptions need, and native subscriptions belong to. */
  user?: string;
  /**
   * Advanced: the app's agent behind the request (wake.actAs), from a
   * verified credential only. The call is then the agent's, and its user is
   * the agent's.
   */
  agent?: Agent;
  /**
   * The MCP client's name, for a request that doesn't carry one: a
   * stateless server never sees initialize, so the app passes the name it
   * knows from its own sign-in (an OAuth registration's client_name, say).
   * Wakeshift reads it to tell which program the agent runs in, and
   * whether the client subscribes to events itself. A name the request
   * carries (initialize's, or its _meta's) wins. Leave it out for a caller
   * that must have no name.
   */
  client?: string;
}

/**
 * attach's options. C is the request context your MCP SDK's handlers get:
 * @modelcontextprotocol/server 2.x's ServerContext (`ctx`), or
 * @modelcontextprotocol/sdk 1.x's RequestHandlerExtra (`extra`).
 */
export interface AttachOptions<C = any> {
  /**
   * Who is behind a request. The app passes identify, returning { user }:
   * its own ID for the person, from its own sign-in (and, on a stateless
   * server, the client's name as `client`). It may be async. Without it,
   * requests have no user, so personal events and list_subscriptions
   * refuse. It gets the request's context as your MCP SDK passes it to
   * handlers: 2.x's ctx (the token in ctx.http?.authInfo), 1.x's extra
   * (extra.authInfo).
   */
  identify?(ctx: C): Identity | undefined | Promise<Identity | undefined>;
  /**
   * Refuse Wakeshift's part of a request from a caller identify gives no
   * user for, as when identify fails: session_key is then never a bearer.
   * Use it whenever the server requires sign-in. Needs identify.
   */
  requireUser?: boolean;
  /**
   * Which of Wakeshift's own tools attach adds: subscribe_events,
   * list_subscriptions and unsubscribe_events unless false, wakeshift_wait
   * only if true: `builtinTools: { subscribe: false, wait: true }`; false
   * turns every one off. list_subscriptions and unsubscribe_events are
   * listed only while a tool on the server is declared with subscribes (or
   * named in wrap) or an event is open to subscribe_events.
   */
  builtinTools?: BuiltinTools | false;
  /**
   * House agents only: whether a call comes from a client that subscribes
   * to the app's events itself (MCP Events), for callers the SDK can't
   * recognize. false leaves it to the SDK's own checks.
   */
  isNativeClient?(ctx: C): boolean;
  /**
   * The server's tools that subscribe, registered elsewhere (a low-level
   * Server's handlers, the MCP SDK's own registerTool, a vendor's
   * catalog), by name, each with the events it subscribes to. tools/list
   * gives each Wakeshift's arguments and the subscribes sentence; its calls
   * reach your handler without those arguments; the handler subscribes with
   * `wake.currentCall()`; and its result gets Wakeshift's block.
   *
   *   wake.attach(server, { identify, wrap: { start_crawl: [crawlFinished] } })
   *
   * For tools named at run time, a function from a tool's name to its
   * events, or undefined for a tool that doesn't subscribe:
   *
   *   wake.attach(server, { wrap: (tool) => (tool.startsWith("run_") ? [runFinished] : undefined) })
   */
  wrap?: Record<string, readonly AnyEvent[]> | ((tool: string) => readonly AnyEvent[] | undefined);
}

/** Which of Wakeshift's own tools attach adds: subscribe, list and unsubscribe on unless false; wait off unless true. */
export interface BuiltinTools {
  /** subscribe_events, for clients that don't speak MCP Events (only for events open to it). */
  subscribe?: boolean;
  /** list_subscriptions. */
  list?: boolean;
  /** unsubscribe_events. */
  unsubscribe?: boolean;
  /**
   * wakeshift_wait, Claude Code's wait tool, where its client takes MCP
   * tasks (protocol 2026-07-28, a stateless server). Off by default: a
   * waiting conversation's Claude Code holds a request to this server
   * (about 20 seconds) and sends the next about a second later, for as long
   * as it waits: on a serverless host, give the function a timeout of at
   * least 30 seconds, and expect one held invocation per waiting conversation.
   */
  wait?: boolean;
}

/** @internal The names of attach's own tools. */
export type BuiltinTool = "subscribe_events" | "unsubscribe_events" | "list_subscriptions";

const BUILTIN_KEYS = { subscribe: "subscribe_events", list: "list_subscriptions", unsubscribe: "unsubscribe_events" } as const;

/** @internal Which of attach's own tools to add. */
export interface ToolChoice {
  builtin: Set<BuiltinTool>;
  wait: boolean;
}

/** @internal builtinTools as the tools to add, refusing a key or value it doesn't know. */
export function toolChoice(builtinTools: BuiltinTools | false | undefined, what = "attach", wait = true): ToolChoice {
  if (builtinTools === false) return { builtin: new Set(), wait: false };
  if (builtinTools !== undefined && (!builtinTools || typeof builtinTools !== "object" || Array.isArray(builtinTools))) {
    throw new UsageError("config", `${what}'s builtinTools is false (none), or { subscribe, list, unsubscribe${wait ? ", wait" : ""} }, each true or false`);
  }
  for (const [k, v] of Object.entries(builtinTools ?? {})) {
    if (!(k in BUILTIN_KEYS) && !(wait && k === "wait")) throw new UsageError("config", `${what}'s builtinTools has no ${k}: its keys are subscribe, list, unsubscribe${wait ? " and wait" : ""}`);
    if (v !== undefined && typeof v !== "boolean") throw new UsageError("config", `${what}'s builtinTools.${k} is true or false`);
  }
  const builtin = new Set<BuiltinTool>();
  for (const [k, name] of Object.entries(BUILTIN_KEYS)) if (builtinTools?.[k as keyof typeof BUILTIN_KEYS] !== false) builtin.add(name);
  return { builtin, wait: builtinTools?.wait === true };
}

// What a client says about itself on initialize (2.x: or the request's
// envelope), or a request's _meta; else the name identify gave (identified).
function clientInfoName(host: Host, meta: Record<string, unknown> | undefined, identified?: string): string {
  const fromInit = host.clientName();
  if (fromInit) return fromInit;
  const info = meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
  if (typeof info?.name === "string" && info.name) return info.name;
  return identified ?? "";
}

/** A client that speaks MCP Events: it declared the extension, or it's one known to (ChatGPT); never a harness Wakeshift wakes itself (BRIDGED_CLIENTS). */
/** @internal */
export function speaksEvents(capabilities: Record<string, unknown> | undefined, clientName: string): boolean {
  if (bridgedClient(clientName, "")) return false;
  const ext = (capabilities?.extensions as Record<string, unknown> | undefined)?.[EXTENSION_KEY];
  return !!ext || clientName.toLowerCase() === "openai-mcp";
}

// The fixed wording of the tools attach adds and of the refusals of a
// native subscribe, the same in every SDK, word for word
// (test/event-tools.json); the compact strings come from wording.ts. {name}
// is a placeholder fill replaces.
/** @internal */
export const EVENT_TOOL_TEXT = {
  a_webhook: "a webhook",
  arguments_argument: COMPACT.arguments_argument,
  conversation: "wakes a conversation",
  end_no_user: "This app couldn't tell who you are, so it can't find that subscription: sign in to it first.",
  ended: "Stopped subscription {id}.",
  event_argument: COMPACT.event_argument,
  event_line: "{name}({arguments}): {description}",
  events_heading: "\n\nEvents:\n",
  id_argument: COMPACT.id_argument,
  item: "- {id}: {event} {arguments}, {destination}, since {since}",
  list: "Your subscriptions to this app's events:\n{lines}\n\nStop one with unsubscribe_events and its id.",
  list_description: COMPACT.list_description,
  list_failed: "Couldn't list your subscriptions: {detail}",
  list_no_user: "This app couldn't tell who you are, so it can't list your subscriptions: sign in to it first.",
  more: "\n(and {n} more: see the event parameter's options)",
  native_no_user: "this app couldn't tell who you are, so it can't hold a subscription for you: sign in to it first",
  native_refused: "you may not subscribe to {event} with these arguments",
  native_bad_voucher: "this voucher can't subscribe to {event}: it was used, it expired, or it's for other arguments",
  native_tools_only: "{event} can be subscribed only through this app's tools, or with the voucher one of them handed you",
  native_tools_only_poll: "{event} can't be polled: it's subscribed to only through this app's tools, with a webhook",
  native_unknown_event: "this app offers no event named {event}",
  need: "Pass the event and its arguments, or a subscription's id from list_subscriptions.",
  no_such_id: "No subscription of yours has the id {id}: it may have ended already.",
  none: "You have no subscriptions to this app's events.",
  not_subscribed: "This conversation wasn't subscribed to {event} {arguments}.",
  notifies: COMPACT.notifies,
  notifies_event: COMPACT.notifies_event,
  notifies_events: COMPACT.notifies_events,
  notifies_events_separator: COMPACT.notifies_events_separator,
  personal_no_user: "{event} is personal, and this app couldn't tell who you are: sign in to it first.",
  polled: "polled by an MCP client",
  refused: "You may not subscribe to {event} with these arguments.",
  renewed: "Already subscribed to {event} {arguments}: renewed.",
  subscribe_description: COMPACT.subscribe_description,
  subscribe_failed: "Couldn't subscribe: {detail}",
  subscribed: "Subscribed to {event} {arguments}.",
  tools_only: "{event} can be subscribed only through this app's tools.",
  unknown_event: "This app offers no event named {event}.",
  unsubscribe_description: COMPACT.unsubscribe_description,
  unsubscribe_failed: "Couldn't unsubscribe: {detail}",
  unsubscribed: "Stopped {event} {arguments}.",
  voucher_argument: "The voucher from {app}'s tool result. Only its tools hand them out.",
  webhook: "delivered to {host}",
} as const;

/** @internal EVENT_TOOL_TEXT[key] with each {name} replaced. */
export function fill(key: keyof typeof EVENT_TOOL_TEXT, vars: Record<string, string | number> = {}): string {
  return EVENT_TOOL_TEXT[key].replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m));
}


/** @internal notifies' sentence, naming app ("this app" if none), with when ("a build finishes") if given. */
export function notifiesSentence(app: string, when?: string): string {
  const w = when?.trim().replace(/[.\s]+$/, "");
  const App = sentenceApp(app);
  return EVENT_TOOL_TEXT.notifies.replace(/\{when\}/g, w ? ` when ${w}` : "").replace(/\{App\}/g, App).replace(/\{app\}/g, app);
}

/**
 * @internal The sentence tools/list adds to a tool that declares the events
 * it subscribes to (subscribes, or wrap): notifies' sentence with
 * the events' own descriptions where when goes, "of this event: <d>" or
 * "of these events: <d1>; <d2>". Each description is on one line, without
 * its trailing period: the sentence adds one. test/subscribes.json pins it.
 */
export function subscribesSentence(app: string, descriptions: readonly string[]): string {
  const ds = eventDescriptions(descriptions);
  if (ds.length === 0) return "";
  const of = fill(ds.length === 1 ? "notifies_event" : "notifies_events", { events: ds.join(EVENT_TOOL_TEXT.notifies_events_separator) });
  const App = sentenceApp(app);
  return EVENT_TOOL_TEXT.notifies.replace(/\{when\}/g, () => of).replace(/\{App\}/g, () => App).replace(/\{app\}/g, () => app);
}

/** @internal The app's name where it starts a sentence: as written, except "this app" (no name), which is capitalized. */
export function sentenceApp(app: string): string {
  return app === UNNAMED_APP ? "This app" : app;
}

// eventDescriptions is the declared events' descriptions as the sentence
// quotes them: each on one line, its trailing periods trimmed, each once.
function eventDescriptions(descriptions: readonly string[]): string[] {
  const out: string[] = [];
  for (const d of descriptions) {
    const c = (d ?? "").replace(/\s+/g, " ").trim().replace(/[.\s]+$/, "");
    if (c && !out.includes(c)) out.push(c);
  }
  return out;
}

/**
 * @internal A tool's description as tools/list gives it for a tool that
 * declares the events it subscribes to: its own (whitespace collapsed),
 * then the subscribes sentence, always (test/subscribes.json
 * compact_cases). No events leaves it as it was. app is the app's name
 * ("this app" until tools/list names it).
 */
export function withSubscribes(description: string, descriptions: readonly string[], app: string = UNNAMED_APP): string {
  const n = subscribesSentence(app.trim() || UNNAMED_APP, descriptions);
  if (!n) return description;
  // The vendor's description as it wrote it (markdown, JSON examples),
  // without trailing whitespace; a multi-line one gets a blank line before
  // the sentence, a one-line one a space.
  const d = (description ?? "").trimEnd();
  if (!d.trim()) return n;
  if (/[\r\n]/.test(d)) return `${d}\n\n${n}`;
  // A one-line description that doesn't end its last sentence gets a
  // period, so the sentence doesn't run on from it.
  return `${d}${/[.!?\u2026]["'\u2019\u201d)\]]*$/.test(d) ? "" : "."} ${n}`;
}

/** @internal The parts of notifies' sentence that name the app, for "this app": tools/list names it. */
export function unnamedNotifiesParts(): [string, string] {
  const s = notifiesSentence(UNNAMED_APP);
  const mid = s.indexOf(" notifies you in this conversation");
  const second = s.indexOf(". This app delivers");
  return [s.slice(0, mid + " notifies you".length), s.slice(second, second + ". This app delivers".length)];
}

/** What notifies' sentence calls an app it isn't told the name of, until tools/list names it. */
export const UNNAMED_APP = "this app";


const MAX_LISTED_EVENTS = 20;

// outputSchemaOf is a copy of one of attach's own tools' output schemas (wording.ts).
function outputSchemaOf(name: BuiltinTool): Record<string, unknown> {
  return JSON.parse(JSON.stringify(OUTPUT_SCHEMAS[name])) as Record<string, unknown>;
}

// rfc3339 is a time as the other SDKs write it: UTC, without fractions of a second.
function rfc3339(s: string): string {
  const t = new Date(s);
  return Number.isNaN(t.getTime()) ? s : t.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** @internal The events part of Wakeshift.attach. */
export function attachEvents<C>(wake: Wakeshift, server: McpServerLike | LowServerLike, events: WakeEvent[], opts: AttachOptions<C> = {}): Attachment<any> {
  const host = hostOf(server);
  if (host.version === 1) checkV1(wake);
  return new Attachment(wake, server, attachHost(wake, host, events, opts));
}

// checkV1 is what attach needs of a process serving @modelcontextprotocol/sdk
// 1.x: require() of ES modules, which loads the ESM copy of the SDK's
// modules it patches (CR M4), and the modules themselves, from 1.23 on (#44,
// #51).
function checkV1(wake: Wakeshift): void {
  const features = (globalThis as { process?: { features?: { require_module?: boolean } } }).process?.features;
  if (features && features.require_module === false) {
    throw new UsageError("config", "attach on @modelcontextprotocol/sdk 1.x needs require() of ES modules: Node 22.12 or later (or 20.19 or later), without --no-experimental-require-module");
  }
  // zod-json-schema-compat arrived in 1.23: its absence says the SDK is older.
  if (optionalModules("@modelcontextprotocol/sdk/types.js").length === 0 || optionalModules("@modelcontextprotocol/sdk/server/zod-json-schema-compat.js").length === 0) {
    wake.log.log("mcp_patch_unavailable", "Wakeshift couldn't load the @modelcontextprotocol/sdk 1.x modules it extends, so protocol 2026-07-28 may not work");
  }
}

/** @internal What attach refuses before it touches a server: no events, or tools it doesn't know. */
const ATTACH_OPTIONS = ["identify", "requireUser", "wrap", "builtinTools", "isNativeClient"];

/** @internal What attach refuses before it touches a server: unknown options or values. With no events it logs no_events. */
export function checkAttach(wake: Wakeshift, events: WakeEvent[], opts: AttachOptions<any>, what = "attach", allowed = ATTACH_OPTIONS): void {
  if (opts === null || typeof opts !== "object") throw new UsageError("config", `${what} takes an options object`);
  for (const k of Object.keys(opts)) {
    if (!allowed.includes(k)) throw new UsageError("config", `${what} has no option ${k}: its options are ${allowed.join(", ")}`);
  }
  for (const k of ["identify", "isNativeClient"] as const) {
    if (opts[k] !== undefined && typeof opts[k] !== "function") throw new UsageError("config", `${what}'s ${k} is a function`);
  }
  if (opts.requireUser !== undefined && typeof opts.requireUser !== "boolean") throw new UsageError("config", `${what}'s requireUser is true or false`);
  toolChoice(opts.builtinTools, what, allowed.includes("isNativeClient"));
  if (events.length === 0) wake.log.log("no_events", `${what} came before any defineEvent, so there's nothing to subscribe to`);
  const wrap = opts.wrap;
  if (wrap !== undefined && typeof wrap !== "function" && (!wrap || typeof wrap !== "object" || Array.isArray(wrap))) {
    throw new UsageError("config", `${what}'s wrap is { [tool]: [events] }, the server's tools that subscribe, each with its events, or a function from a tool's name to its events`);
  }
  for (const [name, list] of Object.entries(typeof wrap === "function" ? {} : (wrap ?? {}))) {
    if (!Array.isArray(list) || list.some((e) => !e || typeof (e as AnyEvent).owner !== "function")) {
      throw new UsageError("config", `${what}'s wrap gives ${name} something other than a list of events: wrap: { ${name}: [event] }, with events from wake.defineEvent`);
    }
  }
}

/** @internal attach on a Host: an McpServer's, a low-level Server's, or a framework adapter's (mastra.ts, tools.ts). Returns its tool layer. */
export function attachHost<C>(wake: Wakeshift, host: Host, events: WakeEvent[], opts: AttachOptions<C> = {}): ToolLayer<C> {
  loadCallStore();
  const choice = toolChoice(opts.builtinTools);
  const tools = choice.builtin;
  // The wait tool, only if the app turns it on and the MCP SDK's request
  // handlers can be reached (tasks.ts): otherwise no call says it takes tasks.
  const wait = choice.wait && !!host.handlers();
  // raw is the MCP SDK's own request context (1.x's extra, 2.x's ctx): what identify gets.
  // identify runs lazily, at most once per request (its promise shared by
  // everything that needs it), and refuses Wakeshift's part of the request
  // if it fails, takes too long, or (requireUser) gives no user.
  if (opts.requireUser && !opts.identify) throw new UsageError("config", "requireUser needs identify, to tell who is calling");
  if (!opts.identify) wake.log.log("no_identify", "attach has no identify, so session_key is a bearer for its conversation");
  const identities = new WeakMap<object, Promise<Caller>>();
  const identityOf = (raw: unknown, tool?: string): Promise<Caller> => {
    const key = raw && typeof raw === "object" ? (raw as object) : undefined;
    const known = key && identities.get(key);
    if (known) return known;
    const p = resolveCaller(wake, opts as AttachOptions<unknown>, raw, tool);
    if (key) identities.set(key, p);
    return p;
  };
  // Who's calling: the user, the client's name (the request's, else identify's), or why it's refused.
  const callerOf = async (raw: unknown): Promise<{ user: string | undefined; client: string; refused?: Refusal; agentToken?: string }> => {
    const c = await identityOf(raw);
    const client = clientInfoName(host, raw ? host.req(raw).meta : undefined, c.ok ? c.client : undefined);
    return c.ok ? { user: c.user, client, agentToken: c.agentToken } : { user: undefined, client, refused: c.reason };
  };
  const layer = new ToolLayer<C>({
    wake,
    host,
    identify: identityOf,
    clientOf: (meta, identified) => clientInfoName(host, meta, identified),
    native: opts.isNativeClient ? (raw) => opts.isNativeClient!(raw as C) === true : undefined,
    wait,
  });
  // Whether a request comes from a client that subscribes itself (MCP
  // Events): the test call.subscribe uses (toolcall.ts nativeOf).
  const nativeRequest = async (raw: unknown): Promise<boolean> => {
    const c = await identityOf(raw);
    return nativeOf(layer.at, raw, c.ok ? c.client : undefined).native;
  };
  if (typeof opts.wrap === "function") layer.wrapWith(opts.wrap);
  else for (const [name, list] of Object.entries(opts.wrap ?? {})) layer.declare(name, checkSubscribes(wake, name, list), { fromWrap: true });
  const byName = new Map(events.map((e) => [e.name, e]));
  // A tool of the server's own named like one of Wakeshift's: Wakeshift's wins, and the vendor hears it (CR L1).
  const conflict = (name: string) => wake.log.log("builtin_name_conflict", `the server's own tool ${name} has the name of one of Wakeshift's tools, so Wakeshift's is served`, { tool: name });
  if (host.registry) host.registry.onConflict = conflict;
  (host as { onConflict?: (name: string) => void }).onConflict = conflict;
  // A server that serves MCP Events itself keeps them: Wakeshift serves
  // none there and never overwrites its capability (#70); its events reach
  // MCP Events clients only through the app's tools. (Mastra's adapter
  // shares events/* instead.)
  const theirs = !host.framework && serverServesEvents(host);
  if (theirs) {
    wake.log.log("events_capability_kept", "this server serves its own MCP Events, so Wakeshift serves none here and leaves its capability as it is");
    layer.at.servesEvents = false;
  } else if (events.length > 0) {
    // Advertised whenever the server has an event: one open to MCP Events
    // clients (personal, authorize or public), or a tools-only one, which a
    // native client subscribes to with the subscribe voucher a tool handed
    // it, while the core hands them out (docs/subscribe-vouchers.md §4.3).
    // The capability is fixed before the core is asked, so it can't wait for
    // that answer: with an old core, events/list lists only open events.
    // For a server whose events are all tools-only it's best-effort: one
    // attached after it connected (fastmcp over stdio) goes on without it,
    // as before vouchers.
    try {
      host.registerCapabilities({ [TOP_LEVEL_KEY]: { listChanged: false }, extensions: { [EXTENSION_KEY]: { listChanged: false } } });
    } catch (err) {
      if (events.some((e) => e.open)) throw capabilitiesError(host, err);
    }
  }
  if (!host.handlers() && !host.framework) {
    // An MCP SDK release that keeps its request handlers elsewhere: the native
    // events and the built-in tools still work, but not what wraps the SDK's own methods.
    wake.log.log("handlers_unreachable", `this MCP SDK keeps its request handlers where Wakeshift can't reach them, so ${choice.wait ? "wakeshift_wait, " : ""}the 2026-07-28 tools/list changes and wrapped tools' output schemas are off`);
  }
  // Clients that speak only protocol 2026-07-28 (OpenClaw): on the MCP SDK
  // 1.x, on a stateless transport, server/discover and resultType
  // (discover.ts); on 2.x, its own.
  serveStateless(host, { bridgedOnly: ["subscribe_events"] });
  // Clients that take MCP tasks there (Claude Code): Wakeshift's wait tool,
  // answered with a task (tasks.ts), if the app turns it on (wait).
  if (wait) serveTasks(wake, host, callerOf);
  // tools/list: a wrapped tool's output schema names wakeshift, which its
  // structured results carry (toolcall.ts); attach's own tools get their
  // output schemas, which an McpServer isn't given (it takes zod).
  layer.ownOutputSchemas(Object.fromEntries([...tools].map((name) => [name, outputSchemaOf(name)])));
  // Nothing on a server whose events are all tools-only and none of whose
  // tools subscribes can make a subscription: it lists no built-in tools.
  if (!events.some((e) => e.open)) layer.hideWhenIdle(["list_subscriptions", "unsubscribe_events"]);
  layer.install();
  // A stdio server ends with its client: the watches end with it (stdin's
  // end, or the transport closing), so nothing keeps the process alive.
  if (!host.framework) closeWithStdio(wake, (host.server as unknown as { server?: unknown }).server ?? host.server);
  // Attached after the client initialized (fastmcp's connect event over
  // stdio comes after initialize): a tools/list it already answered lacked
  // Wakeshift's part, so the client is told to list again.
  host.toolsChangedIfLate?.();

  // ── Native: events/list, events/subscribe, events/unsubscribe ─────────
  // Only the events open to native clients are listed (personal, authorize
  // or public); one only the app's tools subscribe to is refused below.
  const open = events.filter((e) => e.open);
  if (!theirs) serveNative();
  function serveNative(): void {
  // A native request (ChatGPT, or a client that declares MCP Events) also
  // gets the tools-only events, with a required wakeshift_voucher, while the
  // core hands out subscribe vouchers (docs/subscribe-vouchers.md §4.3).
  host.setMethod("events/list", async (_params, raw) => {
    const toolsOnly = events.some((e) => !e.open);
    let app: string | undefined;
    if (toolsOnly && (await nativeRequest(raw)) && (await wake.vouchersOffered())) app = await wake.appName();
    const listed = app === undefined ? open : events;
    return { resultType: RESULT_TYPE, events: listed.map((e) => (e.open ? e.definition() : e.definition(app))) };
  });
  host.setMethod("events/stream", async (_params, raw) => {
    const codes = codesFor(dialectOf(clientInfoName(host, undefined), userAgentOf(host.req(raw))));
    throw new RpcError(codes.unsupported, "push delivery isn't offered: use a webhook (events/subscribe) or poll (events/poll)", { feature: "deliveryMode", value: "stream" });
  });
  type NativeMethod = "subscribe" | "unsubscribe" | "poll";
  const native = async (params: Record<string, any>, raw: unknown, m: NativeMethod): Promise<Record<string, unknown>> => {
    const unsubscribe = m === "unsubscribe";
    // The voucher leaves the arguments before anything reads them (parsing,
    // authorize, onSubscribe, logs, the subscription key).
    const taken = takeVoucher(params?.arguments);
    const p: Record<string, any> = { ...params, arguments: taken.args };
    const req = host.req(raw);
    const meta = (req.meta ?? {}) as Record<string, unknown>;
    const caller = await callerOf(raw);
    const client = caller.client;
    const ua = userAgentOf(req);
    const dialect = dialectOf(client, ua);
    const codes = codesFor(dialect);
    const nameHeader = req.header(NAME_HEADER);
    if (nameHeader && headerValue(nameHeader) !== p.name) {
      throw new RpcError(HEADER_MISMATCH, "Header mismatch: Mcp-Name doesn't match params.name");
    }
    const ev = byName.get(p.name);
    if (!ev) throw new RpcError(codes.notFound, fill("native_unknown_event", { event: JSON.stringify(p.name) }), { kind: "event" });
    // Only the app's tools subscribe to it: refused on every poll, cursor or
    // not (a client-supplied cursor proves nothing), and every subscribe but
    // one with the subscribe voucher a tool handed out, while the core hands
    // them out. An open event ignores a voucher.
    const voucher = !ev.open && m !== "poll" && taken.voucher !== undefined && (await wake.vouchersOffered()) ? taken.voucher : undefined;
    if (!unsubscribe && !ev.open && voucher === undefined) throw new RpcError(codes.forbidden, fill(m === "poll" ? "native_tools_only_poll" : "native_tools_only", { event: ev.name }));
    if (m === "subscribe" && p.delivery?.mode !== "webhook") {
      throw new RpcError(codes.unsupported, 'delivery.mode must be "webhook": the only delivery offered', { feature: "deliveryMode", value: p.delivery?.mode });
    }
    let args: any;
    try {
      args = ev.parseArgs(p.arguments ?? {});
    } catch (err) {
      throw new RpcError(INVALID_PARAMS, detailOf(err));
    }
    if (caller.refused) throw new RpcError(codes.forbidden, refusedText(caller.refused));
    const user = caller.user;
    // With a voucher, no user is needed: the voucher is the authorization,
    // and Wakeshift compares the user when it was handed to one.
    if (!user && voucher === undefined) throw new RpcError(codes.forbidden, fill("native_no_user"));
    const ts = transportSessionOf(req);
    const call = {
      ...(user ? { user } : {}),
      client,
      meta,
      ...(ua ? { user_agent: ua } : {}),
      ...(caller.agentToken ? { agent_token: caller.agentToken } : {}),
      ...(voucher !== undefined && ts ? { transport_session: ts } : {}),
    };
    // A poll has no subscribe step (each poll can start the subscription),
    // so every poll asks authorize: access is checked again each time.
    if (!unsubscribe && !(await ev.allows(subscriberOf(user, client, true, req, raw), args))) {
      throw new RpcError(codes.forbidden, fill("native_refused", { event: ev.name }));
    }
    if (m === "poll") {
      const body: Record<string, unknown> = { call, name: ev.name, arguments: args };
      if (p.cursor != null) body.cursor = p.cursor;
      if (p.maxAgeMs != null) body.max_age_ms = p.maxAgeMs;
      if (p.maxEvents != null) body.max_events = p.maxEvents;
      try {
        const polled = await wake.requestApp<CorePoll>("POST", "/events/poll", body);
        // A poll with no cursor starts a subscription: the core says created.
        if (polled.created) ev.subscribed({ args, user, via: "native", tool: undefined });
        return pollResult(polled);
      } catch (err) {
        throw rpcErrorOf(err, codes, ev.name);
      }
    }
    const body: Record<string, unknown> = { call, name: ev.name, arguments: args, delivery: p.delivery ?? {}, dialect };
    if (voucher !== undefined) body.voucher = voucher;
    try {
      if (unsubscribe) {
        const out = await wake.requestApp<{ ended: boolean }>("POST", "/events/unsubscribe", body);
        if (!out.ended && dialect !== "chatgpt") {
          throw new RpcError(codes.notFound, "no subscription matches that event, arguments and delivery URL", { kind: "subscription" });
        }
        return { resultType: RESULT_TYPE };
      }
      // Absent means null (the draft): none of them goes on as null.
      if (p.ttlMs != null) body.ttl_ms = p.ttlMs;
      if (p.cursor != null) body.cursor = p.cursor;
      if (p.maxAgeMs != null) body.max_age_ms = p.maxAgeMs;
      const { result: res } = await subscribeNatively(wake, ev, body, args, user, voucher);
      return res;
    } catch (err) {
      throw rpcErrorOf(err, codes, ev.name);
    }
  };
  host.setMethod("events/subscribe", (p, raw) => native(p, raw, "subscribe"));
  host.setMethod("events/unsubscribe", (p, raw) => native(p, raw, "unsubscribe"));
  host.setMethod("events/poll", (p, raw) => native(p, raw, "poll"));
  }

  // ── Bridged: subscribe_events, unsubscribe_events, list_subscriptions ─
  // Written in the zod the MCP SDK takes (2.x: zod 4 only).
  const z = host.zod;
  const toolArgs = wakeInputsFor(host);
  const names = events.map((e) => e.name) as [string, ...string[]];
  const listed = open
    .slice(0, MAX_LISTED_EVENTS)
    .map((e) => {
      const props = Object.keys((e.manifest.arguments.properties as object) ?? {}).sort();
      return fill("event_line", { name: e.name, arguments: props.join(", "), description: e.description });
    })
    .join("\n");
  const more = open.length > MAX_LISTED_EVENTS ? fill("more", { n: open.length - MAX_LISTED_EVENTS }) : "";
  const argsInput = z.record(z.string(), z.any()).optional().describe(EVENT_TOOL_TEXT.arguments_argument);
  const result = (text: string, structured: Record<string, unknown>, addition?: ToolAddition): CallToolResult => withAddition(text, addition ?? {}, structured);
  const fail = (text: string): CallToolResult => ({ isError: true, content: [{ type: "text", text }] });
  // register adds one of the tools, if the app wants it (attach's tools).
  // Each built-in has a top-level title (directories check for one), its annotations' title.
  const register = (name: BuiltinTool, config: Parameters<Host["registerTool"]>[1], cb: (input: any, raw: unknown) => Promise<CallToolResult>) =>
    tools.has(name) ? host.registerTool(name, { title: TOOL_ANNOTATIONS[name].title, ...config }, cb) : undefined;
  // subscribe_events takes any of the app's events (so a closed one gets its
  // own answer), and tools/list names only the open ones; a server with no
  // open event has none.
  if (open.length > 0) layer.patchListed("subscribe_events", (t) => openArguments(withEventEnum(t, open.map((e) => e.name))));
  else tools.delete("subscribe_events");
  // The compact tools/list (design §2.12): unsubscribe_events lists only
  // session_key, list_subscriptions none. Both still take all three.
  layer.patchListed("unsubscribe_events", (t) => openArguments(withoutArgs(t, ["harness", "conversation_id"])));
  // list_subscriptions lists session_key too: without a user, it lists the
  // calling conversation's subscriptions, which the key names.
  layer.patchListed("list_subscriptions", (t) => withoutArgs(t, ["harness", "conversation_id"]));
  const subscribeTool = register(
    "subscribe_events",
    {
      description: `${EVENT_TOOL_TEXT.subscribe_description}${EVENT_TOOL_TEXT.events_heading}${listed}${more}`,
      inputSchema: { event: z.enum(names).describe(EVENT_TOOL_TEXT.event_argument), arguments: argsInput, ...toolArgs },
      annotations: { ...TOOL_ANNOTATIONS.subscribe_events },
      outputSchema: outputSchemaOf("subscribe_events"),
    },
    async (input, raw) => {
      const { event, arguments: rawArgs, ...rest } = input as { event: string; arguments?: Record<string, unknown> };
      const ev = byName.get(event);
      if (!ev) return fail(fill("unknown_event", { event }));
      if (!ev.open) return fail(fill("tools_only", { event: ev.name }));
      let args: any;
      try {
        args = ev.parseArgs(rawArgs ?? {});
      } catch (err) {
        return fail(detailOf(err));
      }
      const { user, client, refused, agentToken } = await callerOf(raw);
      if (refused) return fail(refusedText(refused, true));
      if (ev.personal && !user) return fail(fill("personal_no_user", { event: ev.name }));
      if (!(await ev.allows(subscriberOf(user, client, false, host.req(raw), raw), args))) {
        return fail(fill("refused", { event: ev.name }));
      }
      try {
        const out = await wake.requestApp<{ event_subscription: { id: string; created: boolean; arguments?: unknown }; addition: ToolAddition; agent?: unknown }>("POST", "/event-subscriptions", {
          call: toolCallOf(host.req(raw), client, user, rest, "subscribe_events", wait, agentToken),
          name: ev.name,
          arguments: args,
        });
        const what = { event: ev.name, arguments: canonical(args) };
        const created = !!out.event_subscription.created;
        if (created) ev.subscribed({ args, user, via: "subscribe_events", tool: undefined });
        const lead = fill(created ? "subscribed" : "renewed", what);
        // The arguments as the core keeps them (CR L16).
        return result(lead, { subscribed: { event: ev.name, arguments: out.event_subscription.arguments ?? args ?? {}, created } }, out.addition);
      } catch (err) {
        return fail(fill("subscribe_failed", { detail: detailOf(err) }));
      }
    },
  );
  register(
    "unsubscribe_events",
    {
      description: EVENT_TOOL_TEXT.unsubscribe_description,
      inputSchema: {
        event: z.enum(names).optional().describe(EVENT_TOOL_TEXT.event_argument),
        arguments: argsInput,
        id: z.string().optional().describe(EVENT_TOOL_TEXT.id_argument),
        ...toolArgs,
      },
      annotations: { ...TOOL_ANNOTATIONS.unsubscribe_events },
      outputSchema: outputSchemaOf("unsubscribe_events"),
    },
    async (input, raw) => {
      const { event, arguments: rawArgs, id, ...rest } = input as { event?: string; arguments?: Record<string, unknown>; id?: string };
      const { user, client, refused, agentToken } = await callerOf(raw);
      if (refused) return fail(refusedText(refused, true));
      try {
        if (id) {
          // Without a user, the calling conversation's own subscription by its id.
          const who = user ? { user } : { call: toolCallOf(host.req(raw), client, user, rest, "unsubscribe_events", wait, agentToken) };
          const out = await wake.requestApp<{ ended: boolean }>("POST", "/event-subscriptions/end", { ...who, id });
          return result(fill(out.ended ? "ended" : "no_such_id", { id }), { ended: !!out.ended });
        }
        const ev = event ? byName.get(event) : undefined;
        if (!ev) return fail(fill("need"));
        const args = ev.parseArgs(rawArgs ?? {});
        const out = await wake.requestApp<{ ended: boolean; addition?: ToolAddition }>("POST", "/event-subscriptions/unsubscribe", {
          call: toolCallOf(host.req(raw), client, user, rest, "unsubscribe_events", wait, agentToken),
          name: ev.name,
          arguments: args,
        });
        // addition: the conversation's session key stays the same.
        return result(fill(out.ended ? "unsubscribed" : "not_subscribed", { event: ev.name, arguments: canonical(args) }), { ended: !!out.ended }, out.addition);
      } catch (err) {
        return fail(err instanceof UsageError ? detailOf(err) : fill("unsubscribe_failed", { detail: detailOf(err) }));
      }
    },
  );
  // Annotated as in every SDK (TOOL_ANNOTATIONS, test/tool-annotations.json).
  register(
    "list_subscriptions",
    {
      description: EVENT_TOOL_TEXT.list_description,
      inputSchema: { ...toolArgs },
      annotations: { ...TOOL_ANNOTATIONS.list_subscriptions },
      outputSchema: outputSchemaOf("list_subscriptions"),
    },
    async (input, raw) => {
      const { user, client, refused, agentToken } = await callerOf(raw);
      if (refused) return fail(refusedText(refused, true));
      // A user's subscriptions; without one (no identify, or an anonymous
      // caller), the calling conversation's, which its session_key names.
      const who = user ? { user } : { call: toolCallOf(host.req(raw), client, user, (input ?? {}) as Record<string, unknown>, "list_subscriptions", wait, agentToken) };
      try {
        const out = await wake.requestApp<{
          subscriptions?: { id: string; event: string; arguments: unknown; destination: string; webhook_host?: string; created_at?: string }[];
        }>("POST", "/event-subscriptions/list", who);
        out.subscriptions ??= [];
        const structured = {
          subscriptions: out.subscriptions.map((s) => ({ id: s.id, event: s.event, arguments: s.arguments ?? {}, destination: s.destination, since: rfc3339(s.created_at ?? "") })),
        };
        if (out.subscriptions.length === 0) return result(fill("none"), structured);
        const lines = out.subscriptions.map((s) => {
          const where =
            s.destination === "webhook"
              ? fill("webhook", { host: s.webhook_host || EVENT_TOOL_TEXT.a_webhook })
              : s.destination === "poll"
                ? fill("polled")
                : fill("conversation");
          return fill("item", { id: s.id, event: s.event, arguments: JSON.stringify(s.arguments), destination: where, since: rfc3339(s.created_at ?? "").slice(0, 10) });
        });
        return result(fill("list", { lines: lines.join("\n") }), structured);
      } catch (err) {
        return fail(fill("list_failed", { detail: detailOf(err) }));
      }
    },
  );

  // Per-connection negotiation: a client that speaks MCP Events subscribes
  // natively, so it isn't offered subscribe_events (a server made per
  // request never sees initialize, and keeps offering it).
  host.onInitialized(() => {
    // Set both ways: a server reconnected to another client decides again.
    if (speaksEvents(host.clientCapabilities(), host.clientName() ?? "")) subscribeTool?.disable();
    else subscribeTool?.enable();
  });

  return layer;
}


const EVENTS_METHODS = ["events/list", "events/subscribe", "events/unsubscribe", "events/poll", "events/stream"];

// serverServesEvents is whether the server already declares MCP Events: its
// own capability or extension, or events/* handlers (Appwrite's).
function serverServesEvents(host: Host): boolean {
  const caps = host.serverCapabilities();
  const ext = (caps?.extensions as Record<string, unknown> | undefined)?.[EXTENSION_KEY];
  if (caps?.[TOP_LEVEL_KEY] || ext) return true;
  const handlers = host.handlers();
  return !!handlers && EVENTS_METHODS.some((m) => handlers.has(m));
}

// withoutArgs is a tool as tools/list gives it, without the named arguments.
function withoutArgs(tool: Record<string, unknown>, names: string[]): Record<string, unknown> {
  const schema = tool.inputSchema as { properties?: Record<string, unknown>; required?: string[] } | undefined;
  if (!schema?.properties) return tool;
  const properties = Object.fromEntries(Object.entries(schema.properties).filter(([k]) => !names.includes(k)));
  const out: Record<string, unknown> = { ...schema };
  if (Object.keys(properties).length > 0) out.properties = properties;
  else delete out.properties;
  return { ...tool, inputSchema: out };
}

// openArguments is a built-in's arguments object as tools/list gives it:
// any keys, said as additionalProperties: true. zod renders a record of
// anything as additionalProperties: {} (with propertyNames), which some
// clients and schema linters read as false.
function openArguments(tool: Record<string, unknown>): Record<string, unknown> {
  const schema = tool.inputSchema as { properties?: Record<string, Record<string, unknown>> } | undefined;
  const args = schema?.properties?.arguments;
  if (!args) return tool;
  const { propertyNames: _names, additionalProperties: _extra, ...rest } = args;
  return { ...tool, inputSchema: { ...schema, properties: { ...schema.properties, arguments: { ...rest, additionalProperties: true } } } };
}

// withEventEnum is subscribe_events as tools/list gives it: its event
// argument's enum, only the events open to it.
function withEventEnum(tool: Record<string, unknown>, names: string[]): Record<string, unknown> {
  const schema = tool.inputSchema as { properties?: Record<string, Record<string, unknown>> } | undefined;
  const event = schema?.properties?.event;
  if (!event) return tool;
  return { ...tool, inputSchema: { ...schema, properties: { ...schema.properties, event: { ...event, enum: names } } } };
}

/**
 * @internal events/subscribe as Wakeshift takes it (body: the request
 * without anything left to check), and onSubscribe for a subscription it
 * created: a redeemed voucher's is the handing-out tool's, with that call's
 * context if this process still holds it. The testing module's redeem runs
 * this too.
 */
export async function subscribeNatively(
  wake: Wakeshift,
  ev: WakeEvent,
  body: Record<string, unknown>,
  args: unknown,
  user: string | undefined,
  voucher: string | undefined,
): Promise<{ result: Record<string, unknown>; created: boolean }> {
  const out = await wake.requestApp<{ id: string; refresh_before: string; cursor: string; truncated: boolean; delivery_status?: unknown; created?: boolean; via?: string; tool?: string }>(
    "POST",
    "/events/subscribe",
    body,
  );
  const res: Record<string, unknown> = {
    resultType: RESULT_TYPE,
    id: out.id,
    refreshBefore: new Date(out.refresh_before).toISOString().replace(/\.\d{3}Z$/, "Z"),
    cursor: out.cursor,
    truncated: out.truncated,
  };
  if (out.delivery_status) res.deliveryStatus = out.delivery_status;
  if (out.created) {
    // A redeemed voucher: the subscription is the handing-out tool's.
    const tool = typeof out.via === "string" && out.via.startsWith("tool:") ? out.tool || out.via.slice("tool:".length) : undefined;
    if (tool) {
      // Only this exact voucher's context, taken out: a second redemption gets none.
      const context = voucher !== undefined ? takeVoucherContext(wake, voucher) : undefined;
      ev.subscribed(context === undefined ? { args, user, via: "tool", tool } : { args, user, via: "tool", tool, context });
    } else ev.subscribed({ args, user, via: "native", tool: undefined });
  }
  return { result: res, created: !!out.created };
}

// isStdio is whether a transport is either MCP SDK's StdioServerTransport
// (it reads the process's stdin): by shape, so any copy of either SDK counts.
function isStdio(t: unknown): t is { _stdin: { once(ev: string, fn: () => void): unknown }; onclose?: () => void } {
  const x = t as { _stdin?: { once?: unknown }; _stdout?: unknown } | null;
  return !!x && typeof x === "object" && !!x._stdin && typeof x._stdin.once === "function" && !!x._stdout;
}

/**
 * @internal Closes wake (its watches and onSubscribe work) when the server's
 * stdio transport ends: stdin reaching its end, or the transport closing.
 * The server's connect is wrapped on the instance, so a transport connected
 * later is seen; one already connected is hooked at once. Other transports
 * (HTTP, in-memory) are left alone: a session ending there isn't the process's.
 */
export function closeWithStdio(wake: Wakeshift, server: unknown): void {
  const low = server as { connect?: (t: unknown, ...rest: unknown[]) => Promise<unknown>; transport?: unknown } | null;
  if (!low || typeof low !== "object" || typeof low.connect !== "function") return;
  let hooked = false;
  const hook = (t: unknown): void => {
    if (hooked || !isStdio(t)) return;
    hooked = true;
    const stop = () => void wake.close().catch(() => undefined);
    t._stdin.once("end", stop);
    t._stdin.once("close", stop);
    const prev = t.onclose;
    t.onclose = () => {
      prev?.();
      stop();
    };
  };
  try {
    hook(low.transport);
  } catch {
    // a transport getter that throws: nothing to hook
  }
  const connect = low.connect;
  low.connect = function (this: unknown, t: unknown, ...rest: unknown[]) {
    hook(t);
    return connect.call(this, t, ...rest);
  };
}

/** @internal Why identify refused Wakeshift's part of a request: it failed, or requireUser and no user. */
export type Refusal = "failed" | "anonymous";

/** @internal What identify gave for a request: who, or why Wakeshift's part is refused. */
export type Caller = { ok: true; user?: string; client?: string; agentToken?: string } | { ok: false; reason: Refusal };

// resolveCaller runs identify for one request, within its time limit.
async function resolveCaller(wake: Wakeshift, opts: AttachOptions<unknown>, raw: unknown, tool?: string): Promise<Caller> {
  // requireUser and no user: identify answered (or there was nothing to ask), so it's not identify_error.
  const anonymous = (): Caller => {
    wake.log.log("user_required", `requireUser refused ${tool ? tool + "'s" : "a"} caller: identify gave no user`, tool ? { tool } : {});
    return { ok: false, reason: "anonymous" };
  };
  if (!opts.identify || !raw) return opts.requireUser ? anonymous() : { ok: true };
  const failed = (err: unknown): Caller => {
    wake.log.log("identify_error", `identify failed (${errorType(err)}), so Wakeshift's part of the request was refused`, { error_type: errorType(err) });
    return { ok: false, reason: "failed" };
  };
  let id: Identity | undefined;
  try {
    id = (await withTimeout(Promise.resolve().then(() => opts.identify!(raw)), limits.identifyMs)) ?? undefined;
  } catch (err) {
    return failed(err);
  }
  if (id !== undefined && (id === null || typeof id !== "object")) return failed(new TypeError("identify returned something other than an Identity"));
  let user = typeof id?.user === "string" && id.user ? id.user : undefined;
  let token: string | undefined;
  if (id?.agent !== undefined) {
    // Only the SDK's own Agent, from wake.actAs: a plain object never acts as an agent.
    const t = agentToken(id.agent);
    if (!t) return failed(new TypeError("identify's agent isn't one wake.actAs gave"));
    user = id.agent.user();
    token = t.token;
  }
  if (opts.requireUser && !user) return anonymous();
  return { ok: true, user, client: typeof id?.client === "string" && id.client ? id.client : undefined, agentToken: token };
}

/** @internal What a refused caller is told: by a built-in tool (sentence) or a native method. */
export function refusedText(reason: Refusal, sentence = false): string {
  const t = reason === "anonymous" ? "this app needs you to sign in before it can subscribe you to anything" : "this app couldn't verify who you are, so it can't subscribe you to anything now";
  return sentence ? t.charAt(0).toUpperCase() + t.slice(1) + "." : t;
}

/** What Wakeshift adds to the result of one of attach's own tools: text for the agent, _meta for its harness. */
interface ToolAddition {
  text?: string;
  meta?: Record<string, unknown>;
}

// withAddition is one of attach's own tools' results: its text, then
// Wakeshift's, with Wakeshift's _meta, and structured (TOOL_OUTPUT_SCHEMAS)
// with Wakeshift's text in its wakeshift field.
function withAddition(base: string, a: ToolAddition, structured: Record<string, unknown>): CallToolResult {
  const text = (base.trim() + "\n\n" + (a.text ?? "")).trim();
  const out: CallToolResult = { content: text ? [{ type: "text", text }] : [] };
  if (a.meta && Object.keys(a.meta).length > 0) out._meta = { ...a.meta };
  const add = (a.text ?? "").trim();
  out.structuredContent = add ? { ...structured, wakeshift: add } : structured;
  return out;
}

/** @internal What Wakeshift answers a poll (test/mcp-events-wire.json). */
export interface CorePoll {
  events?: Record<string, unknown>[];
  cursor: string;
  truncated: boolean;
  has_more: boolean;
  next_poll_ms: number;
  /** This poll started the subscription. */
  created?: boolean;
}

/** @internal An events/poll result: what Wakeshift answered, in the draft's names. */
export function pollResult(out: CorePoll): Record<string, unknown> {
  return {
    resultType: RESULT_TYPE,
    events: out.events ?? [],
    cursor: out.cursor,
    truncated: !!out.truncated,
    hasMore: !!out.has_more,
    nextPollMs: out.next_poll_ms,
  };
}

// rpcErrorOf is the JSON-RPC error for a failed native call, in the
// client's dialect's codes. A conversation that isn't the caller's is
// Forbidden, whether it exists or not.
/** @internal */
export function rpcErrorOf(err: unknown, codes: Codes, event = ""): RpcError {
  if (err instanceof RpcError) return err;
  if (!(err instanceof APIError) || err instanceof APIConnectionError) return new RpcError(INTERNAL_ERROR, "Wakeshift couldn't be reached");
  switch (err.code) {
    case "bad_voucher":
      // One answer, whatever the reason (docs/subscribe-vouchers.md §6.2).
      return new RpcError(codes.forbidden, fill("native_bad_voucher", { event }));
    case "callback_failed":
      return new RpcError(codes.callbackEndpoint, err.coreMessage, { reason: err.reason ?? "" });
    case "unknown_event":
      return new RpcError(codes.notFound, err.coreMessage, { kind: "event" });
    case "unsupported":
      return new RpcError(codes.unsupported, err.coreMessage, { feature: "deliveryMode" });
    case "subscription_limit":
      return new RpcError(codes.resourceExhausted, err.coreMessage, { limit: "subscriptions" });
    case "subscription_stopped":
      return new RpcError(codes.forbidden, err.coreMessage, { reason: "stopped" });
    case "bad_request":
    case "bad_arguments":
      return new RpcError(INVALID_PARAMS, err.coreMessage);
    case "unknown_session":
    case "other_client":
    case "unknown_conversation":
      return new RpcError(codes.forbidden, "another client of yours holds that subscription");
  }
  return new RpcError(INTERNAL_ERROR, "Wakeshift refused the subscription: " + err.coreMessage);
}

export type { Dialect };
