import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { AnySchema } from "@modelcontextprotocol/sdk/server/zod-compat.js";
import { WakeshiftError } from "./errors.js";
import { WakeEvent, attachEvents, type AttachOptions, type EventOptions, type EventTypeManifest } from "./events.js";
import type { Attached } from "./toolcall.js";
import { parseKey, signAssertion, type AppKey, isTestKey } from "./key.js";
import { retryRefused } from "./retry.js";
import { takesTasks } from "./discover.js";

/** Wakeshift's production core, the default for every key but an older wst_test_ one. Override with `core` or WAKESHIFT_CORE. */
export const DEFAULT_CORE = "https://app.wakeshift.ai";

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
  /** The app key from the console: wst_ and 50 characters (or an older wst_test_… or wst_live_… key). */
  key: string;
  /**
   * Wakeshift's base URL: the console you got the key from. Default:
   * WAKESHIFT_CORE from the environment, else https://app.wakeshift.ai.
   * An older wst_test_… key has no default: it needs one of the two.
   */
  core?: string;
  /** Default: the global fetch. */
  fetch?: typeof fetch;
  /** Per-request timeout in milliseconds. Default 20000. */
  timeoutMs?: number;
  /**
   * Called with a problem that doesn't fail the call but makes it work less
   * well: each time for a failure, such as a failed call's subscription
   * that couldn't be rolled back, and once for each kind of misuse, such as
   * a wrapped tool that returned with a subscribe still in flight. Default:
   * console.warn.
   */
  onWarning?: (message: string) => void;
}

/** @internal How one request to Wakeshift differs: a longer timeout, and a signal that ends it early. */
export interface RequestOptions {
  timeoutMs?: number;
  signal?: AbortSignal;
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
 * client, in which conversation. The wrapper (attached.events) and attach's
 * own tools pack it.
 */
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
 * value: days must be whole ("1.5d" is a bad_request; use "36h").
 */
export type Duration = number | `${number}${"s" | "m" | "h" | "d"}`;

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
  /** Conversations it woke (or will). */
  matched: number;
  /** An earlier emit had this occurrence's id. */
  duplicate: boolean;
  /** User and native recipients with no live subscription it reached (an emit to several recipients only). */
  unmatched: (string | Recipient)[];
  /** Subscription recipients no longer live (or not that user's). */
  ended: (string | Recipient)[];
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

/** An event's publish URL (`event.publishUrl`). */
export interface PublishUrl {
  id: string;
  event?: string;
  url: string;
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
  ttl_days: number;
  /** At most max wake-ups in any seconds, on top of Wakeshift's own limits. */
  wake_limits?: { seconds: number; max: number }[];
  /** What the person should say to their agent once it's connected; at most 120 characters. */
  start_prompt?: string;
}

/** What the app tells Wakeshift it may do (see wake.publishManifest). */
export interface Manifest {
  description?: string;
  agent?: AgentScope;
  /** Event types defined elsewhere, not with defineEvent: Wakeshift gets these, then the defined ones. Listing a defined one is an error. */
  events?: EventTypeManifest[];
  /** Your app wakes its users' agents with what other people and their agents do there (messages, moves): the consent screen says agents of people they don't know can wake theirs. */
  others_can_wake?: boolean;
  /**
   * Your MCP tools (as your server lists them) that agents' programs can run without asking each time:
   * tools that only read, and routine ones the agent calls on every wake that reach only what it already joined.
   * Never one that deletes, spends, or reaches people the user hasn't chosen. When a user connects Claude Code
   * or Cursor CLI, Wakeshift offers to allow exactly these, by name, in the same question as its install.
   * Letters, digits, _ and -; at most 20.
   */
  preapprove_tools?: string[];
}

/** The parts of an MCP tool callback's `extra` that Wakeshift reads. */
export interface ToolExtra {
  _meta?: Record<string, unknown>;
  requestInfo?: { headers?: Record<string, string | string[] | undefined> };
}

// userAgentOf is the User-Agent of the HTTP request a tool call came in
// ("" if there's none, e.g. over stdio).
function userAgentOf(extra: ToolExtra | undefined): string {
  const headers = extra?.requestInfo?.headers;
  if (!headers) return "";
  const v = headers["user-agent"] ?? headers["User-Agent"];
  return (Array.isArray(v) ? v[0] : v) ?? "";
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
  return out;
}

/**
 * @internal The call to one of attach's own tools (subscribe_events and the
 * others): its _meta and User-Agent, the client's name, the user, and
 * session_key, harness and conversation_id from its arguments.
 */
export function toolCallOf(extra: ToolExtra | undefined, client: string, user: string | undefined, args: Record<string, unknown>, tool: string): Call {
  return packCall({
    meta: extra?._meta,
    clientName: client,
    user,
    session: str(args, SESSION_ARG),
    userAgent: userAgentOf(extra),
    harness: str(args, HARNESS_ARG),
    conversation: str(args, CONVERSATION_ARG),
    tool,
    tasks: takesTasks(extra?._meta, extra),
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
  readonly core: string;
  private readonly key: AppKey;
  // The app's ID: from the key, if it names the app, or else from app().
  private knownAppId: string | null;
  private appInfo?: Promise<AppInfo>;
  private readonly fetchFn: typeof fetch;
  private readonly timeoutMs: number;
  private readonly warnFn: (message: string) => void;
  private readonly warned = new Set<string>();

  constructor(opts: WakeshiftOptions) {
    if (!opts?.key) throw new Error("wakeshift: no app key (WAKESHIFT_KEY?)");
    this.key = parseKey(opts.key);
    this.knownAppId = this.key.appId;
    // Test keys come from staging and local consoles, and one sent to
    // production fails in a way that's hard to read: ask for the core.
    const core = opts.core || process.env.WAKESHIFT_CORE || (isTestKey(opts.key) ? "" : DEFAULT_CORE);
    if (!core) {
      throw new Error(
        "wakeshift: no core URL for this test key: set WAKESHIFT_CORE, or pass { core }, to the address of the console the key came from (for example https://app.staging.wakeshift.ai)",
      );
    }
    this.core = core.replace(/\/+$/, "");
    // A request the load balancer refused during a rollout is sent again (retry.ts).
    this.fetchFn = retryRefused(opts.fetch ?? globalThis.fetch);
    this.timeoutMs = opts.timeoutMs ?? 20_000;
    this.warnFn = opts.onWarning ?? ((m) => console.warn(m));
  }

  /**
   * The app's ID. An older key (wst_test_…, wst_live_…) names it; for a
   * current key, which doesn't, this throws until `await wake.app()` has
   * asked Wakeshift.
   */
  get appId(): string {
    if (!this.knownAppId) throw new Error("wakeshift: call await wake.app() first: this key doesn't name its app");
    return this.knownAppId;
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
          throw new WakeshiftError(200, "bad_response", `Wakeshift at ${this.core} didn't say which app this key is`);
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

  /** @internal The app's ID if it's known yet (the key names it, or app() has answered), else null. */
  knownApp(): string | null {
    return this.knownAppId;
  }

  // A fresh assertion of the app's identity, naming the app once it's known.
  private assertion(): string {
    return signAssertion(this.key, this.core, Date.now(), this.knownAppId);
  }

  /** @internal Reports a problem through onWarning, every time. */
  warn(message: string): void {
    this.warnFn(message);
  }

  /** @internal Reports a warning, once per kind. */
  warnOnce(kind: string, message: string): void {
    if (this.warned.has(kind)) return;
    this.warned.add(kind);
    this.warnFn(message);
  }

  /** Stops a publish URL working (410 from then on). */
  async revokePublishUrl(id: string): Promise<void> {
    await this.request("DELETE", `/events/publish-urls/${encodeURIComponent(id)}`);
  }

  /**
   * Deletes everything Wakeshift keeps for one of the app's users: their
   * conversations, subscriptions, events and deliveries, publish URLs for
   * them, and their ID. Open streams close. Returns false if there was nothing to forget.
   */
  async forgetUser(user: string): Promise<boolean> {
    return (await this.request<{ forgotten: boolean }>("POST", "/users/forget", { user })).forgotten;
  }

  /**
   * Tells Wakeshift what the app may do, replacing what it said before: m,
   * with the events you defined (defineEvent) after m's own events. An
   * event both defined and listed in m.events is an error, and nothing is
   * sent. You needn't call it: a process that serves your tools (attach)
   * sends the manifest automatically, merged into the one Wakeshift has.
   * Call it to remove an event or clear a setting. Once called, m's
   * settings are what every automatic send carries (even if this one
   * failed), with the events defined by then. Returns the manifest as
   * Wakeshift keeps it.
   */
  async publishManifest(m: Manifest): Promise<Manifest> {
    const version = this.manifestVersion; // before the body: a later defineEvent is sent again
    const body = this.manifestBody(m);
    this.manifestBase = m;
    const run = this.serial(() => this.request<Manifest>("PUT", "/manifest", body));
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
  private manifestBody(m: Manifest): Manifest {
    const listed = m.events ?? [];
    for (const name of this.events.keys()) {
      if (listed.some((e) => e.name === name)) {
        throw new Error(`wakeshift: event ${name} is defined with defineEvent and listed in the manifest's events: keep one`);
      }
    }
    const events = [...listed, ...[...this.events.values()].map((e) => e.manifest)];
    const { events: _listed, ...rest } = m;
    return events.length > 0 ? { ...rest, events } : rest;
  }

  // The automatic send, from a process that serves the app's tools: the
  // settings the app last published (only those it set), with the defined
  // events, merged into the manifest Wakeshift has, so it never removes an
  // event or clears a setting another process sent.
  private async sendManifest(): Promise<void> {
    const body: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(this.manifestBody(this.manifestBase ?? {}))) {
      if (v === undefined || v === null || v === "" || v === false || (Array.isArray(v) && v.length === 0)) continue;
      body[k] = v;
    }
    if (!body.events) return; // nothing to add (attach refuses with no events, but never send an empty list)
    await this.request("POST", "/manifest/merge", body);
  }

  // ── Events (MCP Events, natively or bridged) ──────────────────────────

  private readonly events = new Map<string, WakeEvent>();
  // What publishManifest was last given (null: nothing yet); whether this
  // process serves the app's tools (attach: only then is the manifest sent
  // automatically); whether Wakeshift has the manifest for the events
  // defined now (the send; null: not yet, or a defineEvent since); and how
  // many defineEvents there were.
  private manifestBase: Manifest | null = null;
  private serving = false;
  private published: Promise<void> | null = null;
  private manifestVersion = 0;

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
  defineEvent<A extends AnySchema, P extends AnySchema>(name: string, opts: EventOptions<A, P>): WakeEvent<A, P> {
    if (this.events.has(name)) throw new Error(`wakeshift: event ${name} is defined twice`);
    const ev = new WakeEvent<A, P>(this, name, opts);
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
   * It returns the handle your own tools use to subscribe the conversation
   * calling them: wrap a tool's callback with attached.events, and give it
   * Wakeshift's arguments with attached.inputs.
   */
  attach(server: McpServer, opts: AttachOptions = {}): Attached {
    const attached = attachEvents(this, server, [...this.events.values()], opts);
    this.serving = true; // the manifest is sent automatically from now on
    return attached;
  }

  /**
   * @internal Sends the manifest with the defined events, once (again after
   * a defineEvent), from a process that serves the app's tools (attach). A
   * process that only emits never sends it: its events may be fewer, or
   * older, than its server's.
   */
  ensureEvents(): Promise<void> {
    if (!this.serving) return Promise.resolve();
    if (!this.published) this.track(this.serial(() => this.sendManifest()), this.manifestVersion);
    return this.published!;
  }

  /** @internal An event's emit. */
  async emitEvent(e: { name: string; arguments: unknown; payload: unknown; text: string; id?: string; to?: string | (string | Recipient)[] }): Promise<Emitted> {
    await this.ensureEvents();
    const body: Record<string, unknown> = { name: e.name, arguments: e.arguments, payload: e.payload, text: e.text };
    if (e.id) body.id = e.id;
    if (e.to) body.to = e.to;
    const out = await this.request<{ matched: number; duplicate?: boolean; unmatched?: unknown; ended?: unknown }>("POST", "/events/emit", body);
    return { matched: out.matched, duplicate: !!out.duplicate, unmatched: recipientsOf(out.unmatched), ended: recipientsOf(out.ended) };
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

  // ── Users' workspaces ──────────────────────────────────────────────────

  /**
   * The workspace Wakeshift keeps for one of the app's users (user: the
   * app's own ID for them, as attach's identify gives it), created on first
   * use. Wakeshift keeps that user's conversations there.
   */
  async workspace(user: string, name: string): Promise<string> {
    return (await this.request<{ workspace: string }>("POST", "/workspaces", { external_id: user, name })).workspace;
  }

  /** Creates an agent in one of the app's workspaces (if new), and its grant for the app's manifest scope. Returns the grant's ID. */
  async addAgent(workspace: string, agent: string, displayName: string): Promise<string> {
    return (await this.request<{ grant: string }>("POST", `/workspaces/${encodeURIComponent(workspace)}/agents`, { agent, display_name: displayName })).grant;
  }

  /**
   * Checks that one of the app's agents still has a live grant, asking
   * Wakeshift each time. Throws a WakeshiftError: no_grant once the grant is
   * revoked, unknown_workspace once the workspace is forgotten.
   */
  async checkAgent(workspace: string, agent: string): Promise<void> {
    await this.request("POST", `/workspaces/${encodeURIComponent(workspace)}/agents/${encodeURIComponent(agent)}/token`);
  }

  private async request<T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
    const headers: Record<string, string> = { Authorization: "Bearer " + this.assertion() };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let resp: Response;
    let text: string;
    const timeoutMs = Math.max(opts.timeoutMs ?? 0, this.timeoutMs);
    const timeout = AbortSignal.timeout(timeoutMs);
    try {
      resp = await this.fetchFn(this.core + "/api/apps/v1" + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: opts.signal ? AbortSignal.any([timeout, opts.signal]) : timeout,
      });
      text = await resp.text();
    } catch (err) {
      throw this.unreachable(err, timeoutMs);
    }
    let json: any;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      json = undefined;
    }
    if (!resp.ok) {
      // Wakeshift's code, message and reason where it sent them as strings, else
      // the body's start, else the status's text, as every SDK reads it (test/errors.json).
      const e = json?.error !== null && typeof json?.error === "object" ? json.error : {};
      const str = (v: unknown) => (typeof v === "string" ? v : "");
      throw new WakeshiftError(resp.status, str(e.code), str(e.message) || text.slice(0, 200) || resp.statusText, {
        reason: str(e.reason) || undefined,
      });
    }
    // A 2xx that isn't Wakeshift's JSON: a proxy's page, or a wrong core.
    if (json === null || typeof json !== "object" || Array.isArray(json)) {
      throw new WakeshiftError(
        resp.status,
        "bad_response",
        `Wakeshift at ${this.core} answered ${resp.status} without a JSON object. Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from`,
      );
    }
    return json as T;
  }

  // A request that got no answer. Name the core it went to: a wrong or
  // missing WAKESHIFT_CORE is the usual cause.
  private unreachable(err: unknown, timeoutMs = this.timeoutMs): WakeshiftError {
    const e = err as { name?: string; message?: string; cause?: { code?: string; message?: string } } | undefined;
    const why =
      e?.name === "TimeoutError" || e?.name === "AbortError"
        ? `no answer within ${timeoutMs / 1000}s`
        : [e?.message, e?.cause?.code ?? e?.cause?.message].filter(Boolean).join(": ") || String(err);
    return new WakeshiftError(
      0,
      "unreachable",
      `can't reach Wakeshift at ${this.core} (${why}). Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from`,
      { cause: err },
    );
  }
}
