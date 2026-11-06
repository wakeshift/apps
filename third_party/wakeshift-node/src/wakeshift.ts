import { createHash, randomUUID } from "node:crypto";
import { closeWatches } from "./watch.js";
import { forgetVoucherContexts } from "./voucherctx.js";
import { APIConnectionError, APIError, IdReusedError, UsageError } from "./errors.js";
import type { AnySchema } from "./zod.js";
import type { HostContext, LowServerLike, McpServerLike, ToolCallExtra } from "./mcp.js";
import { claimServer, v1Req, type Req } from "./host.js";
import { WakeEvent, attachEvents, checkAttach, type AttachOptions, type EventOptions, type RequestOptions } from "./events.js";
import { Attachment, DisabledCall, currentCall, testCallFor, type ToolCall } from "./toolcall.js";
import { parseKey, signAssertion, type AppKey, isTestKey } from "./key.js";
import { RETRY_PAUSES_MS, retryRefused } from "./retry.js";
import { LOG_LEVELS, Log, STDERR_LOGGER, logConfigError, type LogLevel, type Logger } from "./log.js";
import { takesTasks } from "./discover.js";

const INSPECT = Symbol.for("nodejs.util.inspect.custom");

/** Wakeshift's production core, the default for every key but an older wst_test_ one. Override with `core` or WAKESHIFT_CORE. */
export const DEFAULT_CORE = "https://app.wakeshift.ai";

/**
 * @internal How long tools/list waits for the app's name (GET /app) before
 * saying "this app" (waitMs), and how long after a failed or unanswered
 * fetch it doesn't ask again (retryMs). Tests shorten them.
 */
export const nameFetch = { waitMs: 3_000, retryMs: 60_000 };

/** The tool argument an agent passes its session key in (a result gives it one). */
export const SESSION_ARG = "session_key";

/** The tool argument an agent says which program it runs in with. */
export const HARNESS_ARG = "harness";

/**
 * The tool argument an agent passes its own ID for the conversation in,
 * when its program gives it one (Cursor and Grok Bot: CURSOR_CONVERSATION_ID).
 */
export const CONVERSATION_ARG = "conversation_id";

export interface WakeshiftOptions {
  /**
   * The app key from the console: wst_ and 50 characters (or an older
   * wst_test_… or wst_live_… key). Default: WAKESHIFT_KEY from the
   * environment. Without a key the client is disabled: everything works,
   * nothing is sent, and your server runs as it would without Wakeshift.
   * An explicit empty key means no key; it never reads the environment.
   */
  key?: string;
  /** Throw at construction when there's no key, rather than running disabled: for a hosted server that must have one. */
  required?: boolean;
  /**
   * Wakeshift's base URL: the console you got the key from. Default:
   * WAKESHIFT_CORE from the environment, else https://app.wakeshift.ai.
   * https only, except on localhost, 127.0.0.1 and ::1. An older
   * wst_test_… key has no default: it needs one of the two.
   */
  core?: string;
  /** Default: the global fetch, looked up at each request (a test that stubs globalThis.fetch after construction is seen). */
  fetch?: typeof fetch;
  /** Per-request timeout in milliseconds. Default 20000. */
  timeoutMs?: number;
  /** How many times a request the load balancer refused is sent again. Default 2. */
  maxRetries?: number;
  /** Where the SDK logs. Default: stderr, for every level (never stdout, which a stdio MCP server keeps for JSON-RPC). */
  logger?: Logger;
  /** Lines at this level and above are logged. Default "info". */
  logLevel?: LogLevel;
}

const OPTIONS = new Set(["key", "required", "core", "fetch", "timeoutMs", "maxRetries", "logger", "logLevel"]);

// envOf reads the environment where there is one (not every runtime has process).
function envOf(name: string): string | undefined {
  const v = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env?.[name];
  return typeof v === "string" ? v : undefined;
}

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

// checkCore refuses a core that isn't https, except on a loopback host.
function checkCore(core: string): string {
  let url: URL | undefined;
  try {
    url = new URL(core);
  } catch {
    url = undefined;
  }
  const ok = !!url && (url.protocol === "https:" || (url.protocol === "http:" && LOOPBACK.has(url.hostname)));
  if (!ok) throw new UsageError("config", "the core is an https:// URL (http:// only on localhost, 127.0.0.1 or ::1)");
  return core.replace(/\/+$/, "");
}


/** Which app a key is, as Wakeshift says (wake.app()). */
export interface AppInfo {
  id: string;
  name: string;
  /** The key: the console's ID for it, and its name ("" if Wakeshift didn't say). */
  key: { id: string; name: string };
}

/**
 * @internal A tool call as Wakeshift takes it: who called, from which
 * client, in which conversation. A tool that subscribes (attached.registerTool,
 * or named in wrap) and attach's own tools pack it.
 */
/** @internal The core's feature: it hands out subscribe vouchers for tools-only events to native callers. */
export const SUBSCRIBE_VOUCHERS = "subscribe_vouchers";

export interface Call {
  /** The app's own ID for the person (optional). */
  user?: string;
  /** The MCP client's name (initialize's clientInfo.name); a stateless server has none. */
  client: string;
  /** The tool call's _meta (Codex sends its thread there). */
  meta?: Record<string, unknown>;
  /** The session handle an earlier result gave the agent. */
  session?: string;
  /** The MCP request's HTTP User-Agent. */
  user_agent?: string;
  /** The conversation's ID as the agent passed it (conversation_id). */
  conversation?: string;
  /** The program the agent says it runs in (harness): a hint, never a credential. */
  harness?: string;
  /** The name of the tool being called: a connector beacon in its result names it. */
  tool?: string;
  /** The client takes MCP tasks (protocol 2026-07-28 with the tasks extension), and attach serves the wait tool for it (tasks.ts). */
  tasks?: boolean;
  /** The token of the agent the app acts for (identify's agent, from wake.actAs). A secret. */
  agent_token?: string;
  /** For a native caller: the SHA-256 (hex) of the request's Mcp-Session-Id, when it has one. An anonymous subscribe voucher is bound to it. */
  transport_session?: string;
}

const AGENT_TOKEN = new WeakMap<object, { token: string; expires: number }>();
// Each client's app key, outside the object, so no printer reaches it.
const KEYS = new WeakMap<object, AppKey>();

/**
 * One of the app's users' agents, as the app acts for it (wake.actAs),
 * under its grant in the workspace the app keeps for that user. Give it as
 * identify's agent, from a verified credential only. Its token never
 * prints.
 */
export class Agent {
  readonly workspace: string;
  readonly id: string;

  /** @internal Use wake.actAs. */
  constructor(workspace: string, id: string, token: string, expires: number) {
    this.workspace = workspace;
    this.id = id;
    AGENT_TOKEN.set(this, { token, expires });
  }

  /** The agent as the subscriber its calls are made as: "<workspace>/<id>". */
  user(): string {
    return `${this.workspace}/${this.id}`;
  }

  toJSON(): { workspace: string; id: string } {
    return { workspace: this.workspace, id: this.id };
  }

  [INSPECT](): string {
    return `Agent { workspace: ${JSON.stringify(this.workspace)}, id: ${JSON.stringify(this.id)} }`;
  }
}

/** @internal An Agent's token and when it expires (ms); undefined for anything that isn't the SDK's own Agent. */
export function agentToken(a: unknown): { token: string; expires: number } | undefined {
  return a && typeof a === "object" ? AGENT_TOKEN.get(a) : undefined;
}

/**
 * What Wakeshift recognized the calling conversation's harness as, and by
 * which signal ("claim": only the agent's harness argument named it); both
 * null when nothing named one. claimed is what the agent said (normalized:
 * "Grok Bot" is grok-bot), null if it didn't; a claimed that differs from
 * harness was ignored, since it contradicted what the call was recognized
 * as (the claim only refines it: Cursor's calls may be Grok Bot's).
 */
export interface Recognized {
  harness: string | null;
  via: "client" | "meta" | "user_agent" | "claim" | null;
  claimed: string | null;
}

/**
 * Seconds, or a string like "90s", "5m", "2h", "30d". Wakeshift checks the
 * value: days must be whole ("1.5d" is a bad_request; use "36h"). Any string
 * type-checks (a value read from config or typed string); one that isn't a
 * number and s, m, h or d throws when used.
 */
export type Duration = number | `${number}${"s" | "m" | "h" | "d"}` | (string & {});

/**
 * One recipient of a personal event's emit, beside a plain user (the app's
 * ID for them: every subscription they have to the event with these
 * arguments): `{ user, subscription }`, exactly that subscription of that
 * user's; `{ native: user }`, only that user's native (webhook)
 * subscriptions to the event with these arguments.
 */
export type Recipient = { user: string; subscription: string } | { native: string };

/** What an event's emit did. */
export interface Emitted {
  /** Conversations it reached: each woken now, or as soon as its agent can take it. */
  matched: number;
  /**
   * A retry: an earlier emit, the same as this one, had its id, and this is
   * that emit's result (nothing new was sent). The id on a different emit
   * throws IdReusedError instead.
   */
  duplicate: boolean;
  /** User and native recipients with no live subscription it reached, whether `to` names one or many. */
  unmatched: (string | Recipient)[];
  /** Subscription recipients no longer live (or not that user's). */
  ended: (string | Recipient)[];
  /**
   * true: the text was over 1000 bytes, so Wakeshift trimmed it to 1000
   * (ending with "…") and sent the occurrence (logged once per event as
   * text_trimmed).
   */
  textTrimmed: boolean;
}

// recipientsOf reads an emit's unmatched or ended list: "user",
// { native: user } or { user, subscription }.
function recipientsOf(raw: unknown): (string | Recipient)[] {
  if (!Array.isArray(raw)) return [];
  const out: (string | Recipient)[] = [];
  for (const r of raw) {
    if (typeof r === "string") out.push(r);
    else if (r && typeof r === "object") {
      const m = r as Record<string, unknown>;
      if (typeof m.native === "string" && m.native) out.push({ native: m.native });
      else if (typeof m.user === "string") out.push({ user: m.user, subscription: typeof m.subscription === "string" ? m.subscription : "" });
    }
  }
  return out;
}

/**
 * How a connected conversation receives what it's woken by: stream (it holds
 * a connection to Wakeshift open), poll (it asked Wakeshift for messages
 * within the last minute), hooks (Wakeshift's connector on the user's
 * computer delivers to it) or webhook (its client subscribed with MCP
 * Events, and Wakeshift sends to the client's webhook).
 */
export type Via = "stream" | "poll" | "hooks" | "webhook";

/**
 * An event's publish URL (`event.createPublishUrl`): a capability URL that
 * emits the event when a third party posts to it. The URL is a credential:
 * every printer (JSON.stringify, util.inspect) elides its token; pass
 * `.url` explicitly where it's meant to go.
 */
export interface PublishUrl {
  id: string;
  event?: string;
  url: string;
  /** Set when it was made with a signing secret: the secret's fingerprint (the first 12 hex digits of its SHA-256), never the secret. */
  signing?: { fingerprint: string; setAt: string; previousValidUntil?: string };
}

/** @internal A PublishUrl whose printers elide the URL's token. */
export function publishUrlOf(id: string, url: string, event?: string, signing?: PublishUrl["signing"]): PublishUrl {
  const out: PublishUrl = { id, url };
  if (event !== undefined) out.event = event;
  if (signing) out.signing = signing;
  const shown = () => ({ ...out, url: elideToken(url) });
  Object.defineProperty(out, "toJSON", { value: shown, enumerable: false });
  Object.defineProperty(out, INSPECT, { value: () => shown(), enumerable: false });
  return out;
}

// elideToken is a publish URL with its token (the last path segment) elided.
function elideToken(url: string): string {
  return url.replace(/\/[^/?#]+(?=$|[?#])/, "/[redacted]");
}

/** An event pattern a manifest asks for, and what the person is told it's for. */
export interface ScopeEvent {
  pattern: string;
  description: string;
}

/** What the app may do with the agents in the workspaces it keeps (manifest.agent). */
export interface AgentScope {
  wake: ScopeEvent[];
  post: ScopeEvent[];
  /** Days the person's consent lasts. */
  ttlDays: number;
  /** At most max wake-ups in any seconds, on top of Wakeshift's own limits. */
  wakeLimits?: { seconds: number; max: number }[];
  /** What the person should say to their agent once it's connected; at most 120 characters. */
  startPrompt?: string;
}

/** An event type in a manifest, defined elsewhere than with defineEvent. */
export interface ManifestEvent {
  name: string;
  description: string;
  arguments: Record<string, unknown>;
  payload: Record<string, unknown>;
  topic: string;
  filter?: Record<string, string>;
  personal?: boolean;
  deleteOnDelivery?: boolean;
  /** Who may subscribe: "tools" (only the app's tools) or "any". */
  subscribe?: "tools" | "any";
}

/** What the app tells Wakeshift it may do (see wake.publishManifest). */
export interface Manifest {
  description?: string;
  agent?: AgentScope;
  /** Event types defined elsewhere, not with defineEvent: Wakeshift gets these, then the defined ones. Listing a defined one is an error. */
  events?: ManifestEvent[];
  /** Your app wakes its users' agents with what other people and their agents do there (messages, moves): the consent screen says agents of people they don't know can wake theirs. */
  othersCanWake?: boolean;
  /**
   * Your MCP tools (as your server lists them) that agents' programs can run without asking each time:
   * tools that only read, and routine ones the agent calls on every wake that reach only what it already joined.
   * Never one that deletes, spends, or reaches people the user hasn't chosen. When a user connects Claude Code
   * or Cursor CLI, Wakeshift offers to allow exactly these, by name, in the same question as its install.
   * Letters, digits, _ and -; at most 20.
   */
  preapproveTools?: string[];
}

// The manifest's fields on the wire (snake_case), and back.
const MANIFEST_WIRE: Record<string, string> = { othersCanWake: "others_can_wake", preapproveTools: "preapprove_tools" };
const AGENT_WIRE: Record<string, string> = { ttlDays: "ttl_days", wakeLimits: "wake_limits", startPrompt: "start_prompt" };
const EVENT_WIRE: Record<string, string> = { deleteOnDelivery: "delete_on_delivery" };

function rename(o: Record<string, unknown>, names: Record<string, string>, back = false): Record<string, unknown> {
  const map = back ? Object.fromEntries(Object.entries(names).map(([k, v]) => [v, k])) : names;
  return Object.fromEntries(Object.entries(o).map(([k, v]) => [map[k] ?? k, v]));
}

/** @internal A manifest as Wakeshift takes it (snake_case). */
export function manifestToWire(m: Manifest): Record<string, unknown> {
  const out = rename(m as Record<string, unknown>, MANIFEST_WIRE);
  if (m.agent) out.agent = rename(m.agent as unknown as Record<string, unknown>, AGENT_WIRE);
  if (m.events) out.events = m.events.map((e) => rename(e as unknown as Record<string, unknown>, EVENT_WIRE));
  return out;
}

/** @internal A manifest as Wakeshift answers it, in this SDK's names. */
export function manifestFromWire(w: Record<string, unknown>): Manifest {
  const out = rename(w, MANIFEST_WIRE, true) as Manifest;
  if (w.agent && typeof w.agent === "object") out.agent = rename(w.agent as Record<string, unknown>, AGENT_WIRE, true) as unknown as AgentScope;
  if (Array.isArray(w.events)) out.events = w.events.map((e) => rename(e as Record<string, unknown>, EVENT_WIRE, true) as unknown as ManifestEvent);
  return out;
}

/** The parts of an MCP tool callback's `extra` that Wakeshift reads. */
export interface ToolExtra {
  _meta?: Record<string, unknown>;
  requestInfo?: { headers?: Record<string, string | string[] | undefined> };
}

/** @internal The User-Agent of the HTTP request a tool call came in ("" if there's none, e.g. over stdio). */
export function userAgentOf(req: Req): string {
  return req.header("user-agent") || req.header("User-Agent");
}

/** @internal Packs a tool call as Wakeshift takes it: empty fields left out, the agent's claims trimmed. */
export function packCall(c: {
  meta?: Record<string, unknown>;
  clientName: string;
  user?: string;
  session?: string;
  userAgent?: string;
  harness?: string;
  conversation?: string;
  tool?: string;
  tasks?: boolean;
  agentToken?: string;
  transportSession?: string;
}): Call {
  const out: Call = { client: c.clientName ?? "" };
  if (c.user) out.user = c.user;
  if (c.meta && Object.keys(c.meta).length > 0) out.meta = c.meta;
  if (c.session) out.session = c.session;
  if (c.userAgent) out.user_agent = c.userAgent;
  if (c.harness?.trim()) out.harness = c.harness.trim();
  if (c.conversation?.trim()) out.conversation = c.conversation.trim();
  if (c.tool?.trim()) out.tool = c.tool.trim();
  if (c.tasks) out.tasks = true;
  if (c.agentToken) out.agent_token = c.agentToken;
  if (c.transportSession) out.transport_session = c.transportSession;
  return out;
}

/** @internal The SHA-256 (hex) of a request's Mcp-Session-Id header, or undefined without one: what binds an anonymous subscribe voucher to the transport session it was handed out in. */
export function transportSessionOf(req: Req | undefined): string | undefined {
  const id = req?.header("mcp-session-id")?.trim();
  return id ? createHash("sha256").update(id).digest("hex") : undefined;
}

/**
 * @internal The call to one of attach's own tools (subscribe_events and the
 * others): its _meta and User-Agent, the client's name, the user, and
 * session_key, harness and conversation_id from its arguments.
 */
export function toolCallOf(extra: Req | ToolExtra | undefined, client: string, user: string | undefined, args: Record<string, unknown>, tool: string, wait: boolean, agentToken?: string): Call {
  const req = v1Req(extra); // a request already read passes through
  return packCall({
    agentToken,
    meta: req.meta,
    clientName: client,
    user,
    session: str(args, SESSION_ARG),
    userAgent: userAgentOf(req),
    harness: str(args, HARNESS_ARG),
    conversation: str(args, CONVERSATION_ARG),
    tool,
    tasks: wait && takesTasks(req.meta, req),
  });
}

// str is a tool argument's value if it's a string.
function str(args: Record<string, unknown> | undefined, name: string): string | undefined {
  return typeof args?.[name] === "string" ? (args[name] as string) : undefined;
}

/**
 * The Wakeshift client for an app: define the events your app offers,
 * attach them to your MCP server, and emit them when they happen.
 */
export class Wakeshift {
  /** @internal */
  readonly core: string;
  /** @internal The client's log (warnings.json). */
  readonly log: Log;
  // The app's ID: from the key, if it names the app, or else from app().
  private knownAppId: string | null;
  private appInfo?: Promise<AppInfo>;
  // The fetch option; without it, globalThis.fetch as it is at each request (so a test may stub it later).
  private readonly fetchOpt: typeof fetch | undefined;
  private readonly timeoutMs: number;
  /** @internal How many times a request is sent again, by default. */
  readonly maxRetries: number;

  /**
   * A client for the app whose key is opts.key, else WAKESHIFT_KEY. Without
   * a key it's disabled (wake.enabled is false): your server runs as
   * before, and nothing is sent. A malformed key, an unknown option or a
   * core that isn't https throws a UsageError (config). No network request.
   */
  constructor(opts: WakeshiftOptions = {}) {
    if (opts === null || typeof opts !== "object") throw new UsageError("config", "new Wakeshift takes an options object");
    for (const k of Object.keys(opts)) {
      if (!OPTIONS.has(k)) throw new UsageError("config", `new Wakeshift has no option ${k}: its options are ${[...OPTIONS].join(", ")}`);
    }
    if (opts.logLevel !== undefined && !LOG_LEVELS.includes(opts.logLevel)) throw new UsageError("config", `logLevel is one of ${LOG_LEVELS.join(", ")}`);
    const logger = opts.logger ?? STDERR_LOGGER;
    if (!logger || typeof logger.error !== "function" || typeof logger.warn !== "function") throw new UsageError("config", "logger needs error and warn functions");
    this.log = new Log(logger, opts.logLevel ?? "info");
    if (opts.timeoutMs !== undefined && !(typeof opts.timeoutMs === "number" && opts.timeoutMs > 0)) throw new UsageError("config", "timeoutMs is a number of milliseconds over 0");
    if (opts.maxRetries !== undefined && !(Number.isInteger(opts.maxRetries) && opts.maxRetries >= 0)) throw new UsageError("config", "maxRetries is a whole number, 0 or more");
    if (opts.required !== undefined && typeof opts.required !== "boolean") throw new UsageError("config", "required is true or false");
    if (opts.fetch !== undefined && typeof opts.fetch !== "function") throw new UsageError("config", "fetch is a function");
    if (opts.core !== undefined && typeof opts.core !== "string") throw new UsageError("config", "the core is an https:// URL");

    // The key: the option if given (an empty one is no key), else WAKESHIFT_KEY.
    const given = opts.key !== undefined;
    if (given && typeof opts.key !== "string") throw new UsageError("config", "the key option is a string: the app key from the console");
    const raw = (given ? opts.key! : (envOf("WAKESHIFT_KEY") ?? "")).trim();
    const source = given ? "the key option" : "WAKESHIFT_KEY";
    if (!raw && opts.required) throw new UsageError("config", given ? "the key option is empty, and required is set" : "there's no app key: set WAKESHIFT_KEY");
    let key: AppKey | undefined;
    if (raw) {
      try {
        key = parseKey(raw);
      } catch {
        // Never a fragment of the key, nor its length.
        throw new UsageError("config", `${source} isn't a Wakeshift app key: copy it again from the console`) as never;
      }
    }
    // The key never prints: it lives outside the object.
    if (key) KEYS.set(this, key);
    this.knownAppId = key?.appId ?? null;
    // Test keys come from staging and local consoles, and one sent to
    // production fails in a way that's hard to read: ask for the core.
    const core = opts.core?.trim() || envOf("WAKESHIFT_CORE")?.trim() || (key && isTestKey(raw) ? "" : DEFAULT_CORE);
    if (!core) {
      throw new UsageError(
        "config",
        "no core URL for this test key: set WAKESHIFT_CORE, or pass { core }, to the address of the console the key came from (for example https://app.staging.wakeshift.ai)",
      );
    }
    this.core = checkCore(core);
    // A request the load balancer refused during a rollout is sent again (retry.ts).
    this.fetchOpt = opts.fetch;
    this.maxRetries = opts.maxRetries ?? 2;
    this.timeoutMs = opts.timeoutMs ?? 20_000;
    if (!key) this.log.log("no_key", "there's no app key, so Wakeshift is off: this server runs as it would without it");
  }

  /** Whether Wakeshift is on: the client has a key. A disabled client sends nothing. */
  get enabled(): boolean {
    return KEYS.has(this);
  }

  /** What printing the client shows: its app and core, never its key. */
  toJSON(): { app: string | undefined; core: string } {
    return { app: this.knownAppId ?? undefined, core: this.core };
  }

  [INSPECT](): string {
    return `Wakeshift { app: ${JSON.stringify(this.knownAppId ?? undefined) ?? "undefined"}, core: ${JSON.stringify(this.core)} }`;
  }

  /**
   * Advanced: the app's ID, once it's known: an older key (wst_test_…,
   * wst_live_…) names it; for a current key it's known once Wakeshift has
   * said it (the first tools/list does). undefined until then, and on a
   * disabled client.
   */
  get appId(): string | undefined {
    return this.knownAppId ?? undefined;
  }

  /**
   * Which app the key is, as Wakeshift says: its ID and name, and the
   * key's. Asks once
   * (GET /api/apps/v1/app) and remembers the answer; afterwards
   * `wake.appId` works for any key.
   */
  app(): Promise<AppInfo> {
    this.appInfo ??= this.request<{ id?: unknown; name?: unknown; key?: { key_id?: unknown; name?: unknown } }>("GET", "/app")
      .then((r) => {
        if (typeof r.id !== "string" || !r.id) {
          throw new APIError({ status: 200, code: "bad_response", message: `Wakeshift at ${this.core} didn't say which app this key is` });
        }
        const id = this.knownAppId ?? r.id;
        this.knownAppId = id;
        const str = (v: unknown) => (typeof v === "string" ? v : "");
        return { id, name: str(r.name), key: { id: str(r.key?.key_id), name: str(r.key?.name) } };
      })
      .catch((err: unknown) => {
        this.appInfo = undefined; // a later call asks again
        throw err;
      });
    return this.appInfo;
  }

  // When the name fetch for tools/list last failed or went unanswered (ms).
  private nameFailedAt: number | null = null;

  /**
   * @internal The app's name for tools/list (app()'s), or "" when Wakeshift
   * doesn't answer within nameFetch.waitMs: tools/list doesn't wait longer
   * for it, and after a failure doesn't ask again for nameFetch.retryMs, so a
   * slow or failing Wakeshift neither stalls nor multiplies tools/list.
   */
  async appName(): Promise<string> {
    if (this.nameFailedAt !== null && Date.now() - this.nameFailedAt < nameFetch.retryMs) return "";
    let timer: ReturnType<typeof setTimeout> | undefined;
    const late = new Promise<null>((resolve) => {
      timer = setTimeout(() => resolve(null), nameFetch.waitMs);
      timer.unref?.();
    });
    try {
      const info = await Promise.race([this.app(), late]);
      if (info) return info.name.trim();
    } catch {
      // as unanswered
    } finally {
      clearTimeout(timer);
    }
    this.nameFailedAt = Date.now();
    return "";
  }

  /** @internal The app's ID if it's known yet (the key names it, or app() has answered), else null. */
  knownApp(): string | null {
    return this.knownAppId;
  }

  // A fresh assertion of the app's identity, naming the app once it's known.
  private assertion(): string {
    return signAssertion(KEYS.get(this)!, this.core, Date.now(), this.knownAppId);
  }

  /** Stops a publish URL working (410 from then on). */
  async revokePublishUrl(id: string, opts: RequestOptions = {}): Promise<void> {
    await this.request("DELETE", `/events/publish-urls/${encodeURIComponent(id)}`, undefined, opts);
  }

  /**
   * Deletes everything Wakeshift keeps for one of the app's users: their
   * conversations, subscriptions, events and deliveries, publish URLs for
   * them, and their ID. Open streams close. Returns false if there was nothing to forget.
   */
  async forgetUser(user: string, opts: RequestOptions = {}): Promise<boolean> {
    return (await this.request<{ forgotten: boolean }>("POST", "/users/forget", { user }, opts)).forgotten;
  }

  /**
   * Tells Wakeshift what the app may do, replacing what it said before: m,
   * with the events you defined (defineEvent) after m's own events. An
   * event both defined and listed in m.events is an error, and nothing is
   * sent. You needn't call it: the SDK sends the manifest automatically,
   * merged into the one Wakeshift has (a process that serves your tools on
   * its first call about events, any process on an emit of an event
   * Wakeshift doesn't know yet). Call it to remove an event or clear a
   * setting. Once called, m's
   * settings are what every automatic send carries (even if this one
   * failed), with the events defined by then. Returns the manifest as
   * Wakeshift keeps it.
   */
  async publishManifest(m: Manifest, opts: RequestOptions = {}): Promise<Manifest> {
    if (!this.enabled) throw new UsageError("disabled", "Wakeshift is off (no app key), so there's no manifest to publish");
    const version = this.manifestVersion; // before the body: a later defineEvent is sent again
    const body = this.manifestBody(m);
    this.manifestBase = m;
    const run = this.serial(async () => {
      const out = await this.request<Record<string, unknown> & { changed?: unknown; features?: unknown }>("PUT", "/manifest", body, opts);
      this.sends++;
      this.noteChanges(out.changed);
      this.noteFeatures(out.features);
      const { changed: _changed, features: _features, ...manifest } = out;
      return manifestFromWire(manifest);
    });
    this.track(run, version);
    return run;
  }

  // serial runs a send after every send before it, explicit or automatic,
  // so the last manifest Wakeshift gets is the newest.
  private sending: Promise<unknown> = Promise.resolve();
  private serial<T>(send: () => Promise<T>): Promise<T> {
    const run = this.sending.then(send, send);
    this.sending = run.catch(() => undefined);
    return run;
  }

  // track makes a send the one event calls wait for: once it's done,
  // Wakeshift has the manifest for the events defined at version, unless it
  // failed (the next call sends again) or an event was defined since.
  private track(send: Promise<unknown>, version: number): void {
    const p: Promise<void> = send.then(() => {
      if (this.manifestVersion !== version && this.published === p) this.published = null;
    });
    this.published = p;
    p.catch(() => {
      if (this.published === p) this.published = null;
    });
  }

  // The manifest as Wakeshift gets it: m's events, then the defined ones,
  // refusing a name in both.
  private manifestBody(m: Manifest): Record<string, unknown> {
    const listed = m.events ?? [];
    for (const name of this.events.keys()) {
      if (listed.some((e) => e.name === name)) {
        throw new UsageError("config", `event ${name} is defined with defineEvent and listed in the manifest's events: keep one`);
      }
    }
    const { events: _listed, ...rest } = manifestToWire(m);
    const events = [...((manifestToWire({ events: listed }).events as unknown[]) ?? []), ...[...this.events.values()].map((e) => e.manifest)];
    return events.length > 0 ? { ...rest, events } : rest;
  }

  // The automatic send: the settings the app last published (only those it
  // set), with the defined events, merged into the manifest Wakeshift has,
  // so it never removes an event or clears a setting another process sent.
  private async sendManifest(): Promise<void> {
    const body: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(this.manifestBody(this.manifestBase ?? {}))) {
      if (v === undefined || v === null || v === "" || v === false || (Array.isArray(v) && v.length === 0)) continue;
      body[k] = v;
    }
    if (!body.events) return; // nothing to add (attach refuses with no events, but never send an empty list)
    const out = await this.request<{ changed?: unknown; features?: unknown }>("POST", "/manifest/merge", body);
    this.sends++;
    this.noteChanges(out.changed);
    this.noteFeatures(out.features);
  }

  // What the core says it offers, from its last manifest answer (features):
  // an old core says nothing, which is none.
  private features = new Set<string>();
  private noteFeatures(features: unknown): void {
    this.features = new Set(Array.isArray(features) ? features.filter((f): f is string => typeof f === "string") : []);
  }

  /**
   * @internal Whether the core hands out subscribe vouchers
   * (docs/subscribe-vouchers.md): its manifest answer reports
   * subscribe_vouchers. Sends the manifest first if it hasn't been; a
   * failed send is no.
   */
  async vouchersOffered(): Promise<boolean> {
    try {
      await this.ensureEvents();
    } catch {
      return false;
    }
    return this.features.has(SUBSCRIBE_VOUCHERS);
  }

  /**
   * @internal Whether the core's last manifest answer reported
   * subscribe_vouchers, without asking: an older core refuses fields it
   * doesn't know, so the voucher fields go only to one that reported it.
   */
  get vouchersKnown(): boolean {
    return this.features.has(SUBSCRIBE_VOUCHERS);
  }

  // noteChanges logs each event the manifest just closed: from any to tools,
  // with the number of subscriptions the core ended (event_closed). Opening
  // one is audited by the core; nothing to log.
  private noteChanges(changed: unknown): void {
    if (!Array.isArray(changed)) return;
    for (const c of changed) {
      if (!c || typeof c !== "object") continue;
      const { event, from, to, ended } = c as { event?: unknown; from?: unknown; to?: unknown; ended?: unknown };
      if (typeof event !== "string" || from !== "any" || to !== "tools") continue;
      const n = typeof ended === "number" ? ended : 0;
      this.log.log("event_closed", `${event} is now subscribed to only through this app's tools: Wakeshift ended ${n} subscription${n === 1 ? "" : "s"} made otherwise`, { event, ended: n });
    }
  }

  // resendManifest is an emit's answer to unknown_event: Wakeshift doesn't
  // have the event (no process has sent a manifest with it yet, or a
  // publish since removed it), so this process sends its own, merged,
  // unless a send finished since the emit went (seen: the sends then). One
  // send at a time (serial), so emits that fail together send it once.
  private resendManifest(seen: number): Promise<void> {
    return this.serial(async () => {
      if (this.sends !== seen) return;
      const version = this.manifestVersion;
      await this.sendManifest();
      if (this.manifestVersion === version) this.published ??= Promise.resolve();
    });
  }

  // ── Events (MCP Events, natively or bridged) ──────────────────────────

  private readonly events = new Map<string, WakeEvent>();
  // What publishManifest was last given (null: nothing yet); whether this
  // process serves the app's tools (attach: only then is the manifest sent
  // automatically); whether Wakeshift has the manifest for the events
  // defined now (the send; null: not yet, or a defineEvent since); and how
  // many defineEvents there were; and how many manifests Wakeshift has
  // taken from this process.
  private manifestBase: Manifest | null = null;
  private serving = false;
  private published: Promise<void> | null = null;
  private manifestVersion = 0;
  private sends = 0;

  /**
   * Defines one of the app's event types: what a subscriber picks (args),
   * what each occurrence carries (payload), and the line an agent reads
   * first (text). Returns the event, whose emit sends occurrences. Define
   * every event before attach.
   *
   *   const message = wake.defineEvent("chat.message", {
   *     description: "A new message in a channel",
   *     args: z.object({ channel: z.string() }),
   *     payload: z.object({ from: z.string(), text: z.string() }),
   *     text: (m) => `${m.from}: ${m.text}`,
   *   })
   *   await message.emit({ channel: "eng" }, { from: "ada", text: "shipped" })
   */
  defineEvent<N extends string, A extends AnySchema, P extends AnySchema, const Personal extends boolean = false>(name: N, opts: EventOptions<A, P, Personal>): WakeEvent<N, A, P, Personal> {
    if (this.events.has(name)) throw new UsageError("config", `event ${name} is defined twice`);
    const ev = new WakeEvent<N, A, P, Personal>(this, name, opts);
    this.events.set(name, ev as unknown as WakeEvent);
    this.published = null; // the manifest changed: sent again when next needed
    this.manifestVersion++;
    return ev;
  }

  /**
   * Attaches Wakeshift to your MCP server, before you connect it: the one
   * call that connects the two. Today it serves the defined events: MCP
   * Events (events/list, events/subscribe, events/unsubscribe) for clients
   * that speak it, such as ChatGPT, and the subscribe_events tool for
   * everyone else, whom Wakeshift wakes its own ways; plus
   * unsubscribe_events and list_subscriptions for every client, so a person
   * can always see and stop what they're subscribed to.
   *
   *   const attached = wake.attach(server, { identify: (extra) => ({ user: myUser(extra) }) })
   *
   * It returns the attachment: register your tools that subscribe with
   * attached.registerTool (your MCP SDK's own config, plus subscribes), and
   * their handlers get the call as their last argument. Tools registered
   * elsewhere subscribe through the wrap option and wake.currentCall().
   *
   * server is an McpServer from @modelcontextprotocol/server 2.x or
   * @modelcontextprotocol/sdk 1.x, or either SDK's low-level Server (with
   * its handlers set: attach wraps them in place, and its wrap option names
   * the tools that subscribe); identify gets that SDK's own request context
   * (2.x's ctx, 1.x's extra).
   */
  attach<S extends McpServerLike | LowServerLike>(server: S, opts?: AttachOptions<HostContext<S>>): Attachment<S>;
  attach(server: McpServerLike | LowServerLike, opts: AttachOptions<any> = {}): Attachment<any> {
    return logConfigError(this.log, () => {
      claimServer(server);
      checkAttach(this, [...this.events.values()], opts);
      // A disabled client adds nothing: the server runs as the vendor wrote it.
      if (!this.enabled) return new Attachment(this, server, null);
      const attached = attachEvents(this, server, [...this.events.values()], opts);
      this.serving = true; // the manifest is sent automatically from now on
      return attached;
    });
  }

  /**
   * The ToolCall of the call being answered of a tool that subscribes (named
   * in wrap, or registered with attached.registerTool), on any server this
   * client is attached to. Without ctx it's found through AsyncLocalStorage
   * (Cloudflare Workers need the nodejs_compat flag); with ctx, by the
   * request context the server passed the handler. Outside such a call it
   * throws a UsageError with code no_call (check with
   * isWakeshiftError(err, "no_call")); on a disabled client it returns a
   * call that does nothing.
   */
  currentCall<C = ToolCallExtra>(ctx?: C): ToolCall<C> {
    const stand = testCallFor(this);
    if (stand) return stand as ToolCall<C>;
    if (!this.enabled) return new DisabledCall(this, "", ctx as C, null) as unknown as ToolCall<C>;
    return currentCall(this, ctx) as unknown as ToolCall<C>;
  }

  /**
   * Stops every watch (event.watch) and waits for them, and for onSubscribe
   * work in flight, to end. Call it on shutdown, and at the end of a test.
   * Watches started after close are skipped.
   */
  async close(): Promise<void> {
    forgetVoucherContexts(this);
    await closeWatches(this);
  }

  /**
   * @internal For a framework adapter (@wakeshift/sdk/mastra, /tools): the
   * defined events, and from now on this process serves the app's tools,
   * so the manifest is sent automatically, as after attach.
   */
  serveEvents(): WakeEvent[] {
    this.serving = true;
    return [...this.events.values()];
  }

  /**
   * @internal Sends the manifest with the defined events, once (again after
   * a defineEvent), from a process that serves the app's tools (attach). A
   * process that only emits sends it only when Wakeshift doesn't know an
   * event it emits (emitEvent): its events may be fewer, or older, than its
   * server's, and a merge never removes an event it leaves out.
   */
  ensureEvents(): Promise<void> {
    if (!this.serving) return Promise.resolve();
    if (!this.published) this.track(this.serial(() => this.sendManifest()), this.manifestVersion);
    return this.published!;
  }

  /**
   * @internal An event's emit. If Wakeshift doesn't know the event
   * (unknown_event: an emit before any process sent the manifest with it,
   * such as at startup before an agent connects), this process sends its
   * manifest, merged, and emits once more.
   */
  async emitEvent(e: { name: string; arguments: unknown; payload: unknown; text: string; id?: string; to?: string | (string | Recipient)[] }, opts: RequestOptions = {}): Promise<Emitted> {
    await this.ensureEvents();
    const body: Record<string, unknown> = { name: e.name, arguments: e.arguments, payload: e.payload, text: e.text };
    // Retries never double-wake: an emit the SDK may send again carries an
    // id, the caller's or one of its own, the same on every attempt.
    if (e.id) body.id = e.id;
    else if ((opts.maxRetries ?? this.maxRetries) > 0) body.id = "sdk_" + randomUUID();
    if (e.to) body.to = e.to;
    type Out = { matched: number; duplicate?: boolean; unmatched?: unknown; ended?: unknown; text_trimmed?: boolean };
    const seen = this.sends;
    let out: Out;
    try {
      out = await this.request<Out>("POST", "/events/emit", body, opts);
    } catch (err) {
      if (!(err instanceof APIError && err.status === 404 && err.code === "unknown_event")) throw err;
      await this.resendManifest(seen);
      out = await this.request<Out>("POST", "/events/emit", body, opts);
    }
    const emitted: Emitted = { matched: out.matched, duplicate: !!out.duplicate, unmatched: recipientsOf(out.unmatched), ended: recipientsOf(out.ended), textTrimmed: out.text_trimmed === true };
    if (emitted.textTrimmed) {
      this.log.log("text_trimmed", `${e.name}'s text was over 1000 bytes, so Wakeshift trimmed it`, { event: e.name });
    }
    return emitted;
  }

  /**
   * @internal A call to the app API (the events machinery's), after the
   * manifest with the defined events is published. opts: a longer timeout
   * (a held tasks/get), and a signal that ends it early.
   */
  async requestApp<T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
    await this.ensureEvents();
    return this.request<T>(method, path, body, opts);
  }

  /**
   * @internal A POST about one of the app's events (a publish URL, a
   * status): if Wakeshift doesn't know the event yet (a fresh process,
   * before anything attached), this process sends its manifest, merged,
   * and asks once more (#62).
   */
  async requestEvent<T>(path: string, body: unknown, opts: RequestOptions = {}): Promise<T> {
    await this.ensureEvents();
    const seen = this.sends;
    try {
      return await this.request<T>("POST", path, body, opts);
    } catch (err) {
      if (!(err instanceof APIError && err.status === 404 && err.code === "unknown_event")) throw err;
      await this.resendManifest(seen);
      return this.request<T>("POST", path, body, opts);
    }
  }

  // ── Users' workspaces ──────────────────────────────────────────────────

  /**
   * The workspace Wakeshift keeps for one of the app's users (user: the
   * app's own ID for them, as attach's identify gives it), created on first
   * use. Wakeshift keeps that user's conversations there.
   */
  async workspace(w: { user: string; name: string }, opts: RequestOptions = {}): Promise<string> {
    return (await this.request<{ workspace: string }>("POST", "/workspaces", { external_id: w.user, name: w.name }, opts)).workspace;
  }

  // The agents the app acts for, by workspace and agent, until shortly before their tokens expire.
  private readonly agents = new Map<string, Agent>();

  /**
   * Acts for one of the app's users' agents, once its grant is live: the
   * Agent to give as identify's agent. Cached until shortly before its
   * token expires.
   */
  async actAs(who: { workspace: string; agent: string }, opts: RequestOptions = {}): Promise<Agent> {
    if (!this.enabled) throw new UsageError("disabled", "Wakeshift is off (no app key), so there's no agent to act for");
    const key = who.workspace + "/" + who.agent;
    const cached = this.agents.get(key);
    if (cached && (agentToken(cached)?.expires ?? 0) - Date.now() > 30_000) return cached;
    return this.actAsFresh(who.workspace, who.agent, opts);
  }

  private async actAsFresh(workspace: string, agent: string, opts: RequestOptions = {}): Promise<Agent> {
    const out = await this.request<{ access_token?: string; expires_in?: number }>(
      "POST",
      `/workspaces/${encodeURIComponent(workspace)}/agents/${encodeURIComponent(agent)}/token`,
      undefined,
      opts,
    );
    const a = new Agent(workspace, agent, out.access_token ?? "", Date.now() + (out.expires_in ?? 0) * 1000);
    for (const [k, v] of this.agents) if ((agentToken(v)?.expires ?? 0) < Date.now()) this.agents.delete(k);
    this.agents.set(workspace + "/" + agent, a);
    return a;
  }

  /** Creates an agent in one of the app's workspaces (if new), and its grant for the app's manifest scope. Returns the grant's ID. */
  async addAgent(a: { workspace: string; agent: string; displayName: string }, opts: RequestOptions = {}): Promise<string> {
    return (await this.request<{ grant: string }>("POST", `/workspaces/${encodeURIComponent(a.workspace)}/agents`, { agent: a.agent, display_name: a.displayName }, opts)).grant;
  }

  /**
   * Checks that one of the app's agents still has a live grant, asking
   * Wakeshift each time. Throws an APIError: no_grant once the grant is
   * revoked, unknown_workspace once the workspace is forgotten.
   */
  async checkAgent(workspace: string, agent: string, opts: RequestOptions = {}): Promise<void> {
    await this.actAsFresh(workspace, agent, opts);
  }

  private async request<T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
    if (!KEYS.has(this)) throw new UsageError("disabled", "Wakeshift is off (no app key), so this call has nothing to ask it");
    if (opts.signal?.aborted) throw opts.signal.reason;
    const retries = opts.maxRetries ?? this.maxRetries;
    // A request the load balancer refused during a rollout is sent again
    // (retry.ts), and so is one whose connection failed: a body that could
    // wake twice (an emit) carries an id by then.
    const fetchFn = retryRefused(this.fetchOpt ?? (((...a: Parameters<typeof fetch>) => globalThis.fetch(...a)) as typeof fetch), RETRY_PAUSES_MS.slice(0, retries));
    const timeoutMs = opts.timeoutMs ?? this.timeoutMs;
    let resp: Response;
    let text: string;
    for (let attempt = 0; ; attempt++) {
      const headers: Record<string, string> = { Authorization: "Bearer " + this.assertion() };
      if (body !== undefined) headers["Content-Type"] = "application/json";
      // The SDK's own timer, not AbortSignal.timeout: Node 22 unrefs that
      // one's timer, so a process with nothing else pending (a fetch that
      // holds no socket) exits mid-request instead of timing out.
      const timeoutCtl = new AbortController();
      const timeout = timeoutCtl.signal;
      const timer = setTimeout(() => timeoutCtl.abort(new DOMException("The operation was aborted due to timeout", "TimeoutError")), timeoutMs);
      try {
        resp = await fetchFn(this.core + "/api/apps/v1" + path, {
          method,
          headers,
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: opts.signal ? AbortSignal.any([timeout, opts.signal]) : timeout,
        });
        text = await resp.text();
        break;
      } catch (err) {
        // The caller's own abort is theirs: its reason, unchanged.
        if (opts.signal?.aborted) throw opts.signal.reason;
        if (timeout.aborted) throw this.timedOut(err);
        if (attempt >= retries) throw this.unreachable(err);
        clearTimeout(timer);
        await new Promise((r) => setTimeout(r, RETRY_PAUSES_MS[Math.min(attempt, RETRY_PAUSES_MS.length - 1)]));
      } finally {
        clearTimeout(timer);
      }
    }
    let json: any;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      json = undefined;
    }
    const requestId = resp.headers.get("x-request-id") || undefined;
    if (!resp.ok) {
      // Wakeshift's code, message and reason where it sent them as strings, else
      // the body's start, else the status's text, as every SDK reads it (test/errors.json).
      const e = json?.error !== null && typeof json?.error === "object" ? json.error : {};
      const str = (v: unknown) => (typeof v === "string" ? v : "");
      const code = str(e.code);
      const message = str(e.message) || text.slice(0, 200) || resp.statusText;
      if (resp.status === 409 && code === "id_reused") throw new IdReusedError({ message, requestId: str(e.request_id) || requestId });
      if (resp.status === 401) this.log.log("key_refused", `Wakeshift refused this app's key (${resp.status})`);
      throw new APIError({ status: resp.status, code, message, reason: str(e.reason) || undefined, requestId: str(e.request_id) || requestId });
    }
    // A 2xx that isn't Wakeshift's JSON: a proxy's page, or a wrong core.
    if (json === null || typeof json !== "object" || Array.isArray(json)) {
      throw new APIError({
        status: resp.status,
        code: "bad_response",
        message: `Wakeshift at ${this.core} answered ${resp.status} without a JSON object. Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from`,
        requestId,
      });
    }
    return json as T;
  }

  // A request that got no answer within the SDK's own timeout: the outcome
  // is unknown, so a retry should carry the same id.
  private timedOut(err: unknown): APIConnectionError {
    return new APIConnectionError({
      code: "timeout",
      message: `wakeshift: Wakeshift at ${this.core} didn't answer in time. The outcome is unknown; an emit sent again with the same id emits once. https://wakeshift.ai/docs/errors#timeout`,
      cause: err,
    });
  }

  // A request that got no answer. Name the core it went to: a wrong or
  // missing WAKESHIFT_CORE is the usual cause.
  private unreachable(err: unknown): APIConnectionError {
    const e = err as { name?: string; message?: string; cause?: { code?: string; message?: string } } | undefined;
    const why = [e?.message, e?.cause?.code ?? e?.cause?.message].filter(Boolean).join(": ") || String(err);
    return new APIConnectionError({
      code: "unreachable",
      message: `wakeshift: can't reach Wakeshift at ${this.core} (${why}). Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from. https://wakeshift.ai/docs/errors#unreachable`,
      cause: err,
    });
  }
}
