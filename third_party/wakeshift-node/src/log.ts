// What the SDK logs, and how often (test/warnings.json): each line has a
// code shared by every SDK, a level, structured fields and a message that
// ends with the code's fix. Misuse logs once per client; failures that may
// go on (an outage, a broken identify) log at most once per five minutes
// per code, with the count of occurrences since the last line, so they stay
// visible without flooding. Lines never carry a key, a session key, a
// token, a header, a signing secret, a publish URL's token, a payload, tool
// arguments, a request body or the vendor's own error messages: only codes,
// names and error types.

/** How much the SDK logs: lines at this level and above. "off" logs nothing. */
export type LogLevel = "debug" | "info" | "warn" | "error" | "off";

/**
 * A line's structured fields: its code always, and what the line is about.
 * A type, not an interface, so a logger taking Record<string, unknown> (pino,
 * winston, console) assigns without a cast.
 */
export type LogFields = {
  code: string;
  event?: string;
  tool?: string;
  reason?: string;
  ended?: number;
  /** For a rate-limited line: the occurrences it stands for. */
  count?: number;
  /** The error's type, never its message. */
  error_type?: string;
};

/**
 * Where the SDK logs: stderr by default (console.error and console.warn, for
 * every level, so a stdio MCP server's stdout carries only JSON-RPC). pino and similar loggers take
 * (fields, message); adapt them in one line:
 * `{ warn: (m, f) => log.warn(f, m), error: (m, f) => log.error(f, m), info: (m, f) => log.info(f, m) }`.
 */
export interface Logger {
  error(message: string, fields: LogFields): void;
  warn(message: string, fields: LogFields): void;
  info?(message: string, fields: LogFields): void;
  debug?(message: string, fields: LogFields): void;
}

/**
 * @internal The default logger: every level to stderr. console.info and
 * console.debug write to stdout in Node, which on a stdio MCP server
 * corrupts the JSON-RPC stream.
 */
export const STDERR_LOGGER: Logger = {
  error: (m, f) => console.error(m, f),
  warn: (m, f) => console.warn(m, f),
  info: (m, f) => console.error(m, f),
  debug: (m, f) => console.error(m, f),
};

type Level = Exclude<LogLevel, "off">;
type Policy = { kind: "once"; per?: (keyof LogFields)[] } | { kind: "rate_limited"; windowMs: number } | { kind: "each" };

const ONCE: Policy = { kind: "once" };
const FIVE_MINUTES: Policy = { kind: "rate_limited", windowMs: 300_000 };

// test/warnings.json codes: level, fix and policy. The last few are this
// SDK's own degraded modes, which no other SDK has.
const CODES: Record<string, { level: Level; fix: string; policy: Policy }> = {
  no_key: { level: "debug", fix: "Set WAKESHIFT_KEY to turn Wakeshift on.", policy: ONCE },
  no_identify: { level: "debug", fix: "Add identify to tie subscriptions to signed-in users, and requireUser to refuse anonymous callers.", policy: ONCE },
  no_events: { level: "warn", fix: "Define events with defineEvent before attach.", policy: ONCE },
  text_trimmed: { level: "warn", fix: "Keep text to a short headline and put the details in the payload.", policy: { kind: "once", per: ["event"] } },
  rollback_failed: { level: "warn", fix: "Nothing to change; Wakeshift retries the cleanup.", policy: FIVE_MINUTES },
  authorize_failed: { level: "error", fix: "Fix authorize: it threw, failed or took over 10 seconds, so the subscription was refused.", policy: FIVE_MINUTES },
  identify_error: {
    level: "error",
    fix: "Make identify return within 10 seconds: return no user for a credential it can't verify, and throw only when the upstream can't answer.",
    policy: FIVE_MINUTES,
  },
  user_required: { level: "info", fix: "Nothing to change if anonymous callers are meant to be refused; otherwise sign the caller in, or turn requireUser off.", policy: FIVE_MINUTES },
  native_unavailable: { level: "info", fix: "To serve MCP Events clients, give the event personal, authorize or public, and sign callers in.", policy: { kind: "once", per: ["tool", "reason"] } },
  event_closed: { level: "warn", fix: "Add authorize, personal or public: true to reopen it.", policy: { kind: "each" } },
  wrap_unknown_tool: { level: "warn", fix: "Check the tool's name in wrap.", policy: { kind: "once", per: ["tool"] } },
  wrap_arg_conflict: { level: "error", fix: "Rename the tool's session_key, harness, conversation_id or wakeshift field.", policy: { kind: "once", per: ["tool"] } },
  description_too_long: { level: "warn", fix: "Shorten the tool's description: Claude Code keeps only its first 2048 characters, which cuts off Wakeshift's sentence.", policy: { kind: "once", per: ["tool"] } },
  wrap_bad_event: { level: "error", fix: "Return only events defined on this client, never null, from the wrap function.", policy: { kind: "once", per: ["tool"] } },
  builtin_name_conflict: { level: "error", fix: "Rename your tool, or turn the built-in off with builtinTools.", policy: { kind: "once", per: ["tool"] } },
  events_capability_kept: {
    level: "info",
    fix: "Nothing to change if you meant to serve your own MCP Events; Wakeshift's events reach MCP Events clients only through your tools.",
    policy: ONCE,
  },
  on_subscribe_failed: { level: "error", fix: "Fix the event's onSubscribe: it threw or rejected; the subscription stands.", policy: FIVE_MINUTES },
  watch_failed: { level: "warn", fix: "Check the watch's check function and the emit; the watch stopped without emitting.", policy: FIVE_MINUTES },
  watch_stopped: { level: "info", fix: "Nothing to change: the check's error was marked final, so the watch stopped as asked.", policy: FIVE_MINUTES },
  watch_timeout: { level: "info", fix: "Raise the watch's timeout if the work can take longer.", policy: { kind: "once", per: ["event"] } },
  key_refused: { level: "error", fix: "Check WAKESHIFT_KEY: the key was revoked, or belongs to another console.", policy: FIVE_MINUTES },
  subscribe_in_flight: { level: "warn", fix: "Await subscribe, unsubscribe and resolveSession before the tool returns.", policy: { kind: "once", per: ["tool"] } },
  mcp_patch_unavailable: { level: "warn", fix: "Upgrade @modelcontextprotocol/sdk to 1.23 or later.", policy: ONCE },
  monitor_line_long: {
    level: "warn",
    fix: "Keep the event's text under 200 bytes and put the details in the payload: Claude Code's Monitor shows a 480-byte line, about 230 of which are Wakeshift's own fields.",
    policy: { kind: "once", per: ["event"] },
  },
  config: { level: "error", fix: "Fix the configuration the message names; the same error was thrown.", policy: { kind: "each" } },
  block_failed: { level: "warn", fix: "Nothing to change; the tool's result went out as the tool gave it, without Wakeshift's addition.", policy: FIVE_MINUTES },
  // This SDK's own.
  tasks_unrouted: { level: "warn", fix: "Upgrade @modelcontextprotocol/server to 2.3.0 or later.", policy: ONCE },
  handlers_unreachable: { level: "warn", fix: "Check for a newer @wakeshift/sdk.", policy: ONCE },
  not_a_tool_result: { level: "warn", fix: "Answer a tool that subscribes in one call, not with a task or an input request.", policy: { kind: "once", per: ["tool"] } },
  mastra_attach_failed: { level: "warn", fix: "Check for an @wakeshift/sdk that supports this @mastra/mcp.", policy: ONCE },
};

const RANK: Record<LogLevel, number> = { debug: 10, info: 20, warn: 30, error: 40, off: 100 };

/** @internal The log levels, for checking an option. */
export const LOG_LEVELS: readonly LogLevel[] = ["debug", "info", "warn", "error", "off"];

/** @internal A code's fix clause, as every message for it ends. */
export function logFixOf(code: string): string | undefined {
  return CODES[code]?.fix;
}

/** @internal One client's log: its logger, level and the once and rate-limited state. */
export class Log {
  /** @internal The clock, for rate limits (tests replace it). */
  now: () => number = () => Date.now();
  private readonly seen = new Set<string>();
  private readonly limited = new Map<string, { at: number; suppressed: number }>();

  constructor(
    private readonly logger: Logger,
    private readonly level: LogLevel,
  ) {}

  /** Logs code (with fields) saying what happened, by the code's policy; the message ends with its fix. */
  log(code: string, what: string, fields: Omit<LogFields, "code"> = {}): void {
    const c = CODES[code];
    const level: Level = c?.level ?? "warn";
    if (RANK[level] < RANK[this.level]) return;
    const out: LogFields = { code, ...fields };
    const policy = c?.policy ?? ONCE;
    if (policy.kind === "once") {
      const key = code + "\u0000" + (policy.per ?? []).map((f) => String((out as unknown as Record<string, unknown>)[f] ?? "")).join("\u0000");
      if (this.seen.has(key)) return;
      this.seen.add(key);
    } else if (policy.kind === "rate_limited") {
      const now = this.now();
      const last = this.limited.get(code);
      if (last && now - last.at < policy.windowMs) {
        last.suppressed++;
        return;
      }
      out.count = (last?.suppressed ?? 0) + 1;
      this.limited.set(code, { at: now, suppressed: 0 });
    }
    const said = what.trim().replace(/^wakeshift: /, "").replace(/[.\s]+$/, "");
    const message = `wakeshift: ${said}.${c?.fix ? " " + c.fix : ""}`;
    const fn = (level === "error" ? this.logger.error : level === "warn" ? this.logger.warn : level === "info" ? this.logger.info : this.logger.debug)?.bind(this.logger);
    try {
      fn?.(message, out);
    } catch {
      // A logger that throws doesn't break the call that logged.
    }
  }
}

/**
 * @internal Runs fn, logging a configuration error it throws (code config)
 * before throwing it on: a framework may turn the throw into a bare HTTP 500
 * (createMcpHandler) where nobody sees it.
 */
export function logConfigError<T>(log: Log, fn: () => T): T {
  try {
    return fn();
  } catch (err) {
    const e = err as { name?: unknown; code?: unknown; message?: unknown } | null;
    if (e && e.code === "config" && typeof e.message === "string") log.log("config", e.message);
    throw err;
  }
}
