// One error hierarchy, the same names in every SDK (test/errors.json codes):
//
//   WakeshiftError            everything the SDK throws
//   ├─ APIError               talking to Wakeshift failed: the core answered with an error
//   │  ├─ APIConnectionError  or didn't answer (unreachable, timeout); status null
//   │  └─ IdReusedError       an emit's id was used for a different emit
//   └─ UsageError             caught before sending: bad arguments, configuration, misuse
//
// Every message the SDK writes itself says what failed, the fix in one
// clause and a docs anchor: "wakeshift: {what}. {fix} {anchor}". Messages
// never carry a secret, a payload, tool arguments or the vendor's own error
// messages.

/** The brand every Wakeshift error carries, so isWakeshiftError works across duplicate installs of the SDK. */
const BRAND = Symbol.for("wakeshift.error");

/** Where the docs explain each code. */
const ERRORS_DOCS = "https://wakeshift.ai/docs/errors#";

type ClassName = "UsageError" | "APIError" | "APIConnectionError" | "IdReusedError";

interface CodeInfo {
  /** The one-clause fix every message for the code carries. */
  fix: string;
  /** The class the SDK raises before sending (null: never). */
  before: ClassName | null;
  /** The class for the core's answer (null: never). */
  core: ClassName | null;
}

// test/errors.json codes, in its order: the API's, then the SDK's own.
const CODES = {
  bad_request: { fix: "Check the call against the API reference.", before: "UsageError", core: "APIError" },
  bad_arguments: { fix: "Pass arguments and a payload that match the event's schemas.", before: "UsageError", core: "APIError" },
  bad_payload: { fix: "Send a payload that matches the event's payload schema.", before: null, core: "APIError" },
  unknown_event: { fix: "Define the event in this app with defineEvent; the SDK sends the manifest itself.", before: "UsageError", core: "APIError" },
  unknown_session: { fix: "Use the session_key from this conversation's own results.", before: null, core: "APIError" },
  unknown_conversation: { fix: "Use the session_key from this conversation's own results.", before: null, core: "APIError" },
  other_client: { fix: "Only the client that made a subscription can change it.", before: null, core: "APIError" },
  private_topic: { fix: "Subscribe only to topics this app and caller may read.", before: null, core: "APIError" },
  subscription_limit: { fix: "End subscriptions that are no longer needed.", before: null, core: "APIError" },
  subscription_stopped: { fix: "The person stopped this subscription; subscribe again only if they ask.", before: null, core: "APIError" },
  id_reused: { fix: "Use a new id for a different emit; reuse an id only to retry the same emit.", before: null, core: "IdReusedError" },
  rate_limited: { fix: "Slow down, and retry after the Retry-After interval.", before: null, core: "APIError" },
  too_large: { fix: "Keep text under 1000 bytes and the payload under 64 KB.", before: null, core: "APIError" },
  unauthenticated: { fix: "Check WAKESHIFT_KEY: the key was revoked, or belongs to another console.", before: null, core: "APIError" },
  no_grant: { fix: "Ask the user to grant this app access first.", before: null, core: "APIError" },
  unknown_workspace: { fix: "Use a workspace ID from wake.workspace.", before: null, core: "APIError" },
  unknown_publish_url: { fix: "Create a new publish URL: this one was revoked or never existed.", before: null, core: "APIError" },
  bad_signing_secret: { fix: "Pass a Standard Webhooks secret: whsec_ and base64.", before: "UsageError", core: "APIError" },
  unsupported: { fix: "Upgrade the SDK, or leave out the option this Wakeshift doesn't support.", before: null, core: "APIError" },
  internal: { fix: "Retry later; if it persists, contact Wakeshift with the request ID.", before: null, core: "APIError" },
  bad_response: { fix: "Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from.", before: null, core: "APIError" },
  callback_failed: { fix: "Make the webhook URL answer Wakeshift's verification with a 2xx.", before: null, core: "APIError" },
  unreachable: { fix: "Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from.", before: null, core: "APIConnectionError" },
  timeout: { fix: "The outcome is unknown; an emit sent again with the same id emits once.", before: null, core: "APIConnectionError" },
  config: { fix: "Fix the option or value the message names.", before: "UsageError", core: null },
  disabled: { fix: "Set WAKESHIFT_KEY, or check wake.enabled before this call.", before: "UsageError", core: null },
  undeclared_subscribe: { fix: "Add the event to the tool's subscribes.", before: "UsageError", core: null },
  no_call: { fix: "Call it inside a tool call that subscribes, or pass the request's context.", before: "UsageError", core: null },
  call_ended: { fix: "Await subscribe, unsubscribe and resolveSession before the tool returns.", before: "UsageError", core: null },
  identify_failed: { fix: "Check identify: it threw, failed or took over 10 seconds.", before: "UsageError", core: null },
  user_required: { fix: "Nothing to change if anonymous callers are meant to be refused; otherwise sign the caller in.", before: "UsageError", core: null },
} as const satisfies Record<string, CodeInfo>;

/** Every error code the SDK and the HTTP API share (test/errors.json codes). */
export type KnownCode = keyof typeof CODES;

/** An error's code: one of the known codes, or a newer one the core sends. */
export type WakeshiftErrorCode = KnownCode | (string & {});

/** @internal The known codes, in errors.json's order. */
export const KNOWN_CODES = Object.keys(CODES) as KnownCode[];

/** @internal The classes errors.json lists for a code: [before sending, from the core]. */
export function classesOf(code: KnownCode): [ClassName | null, ClassName | null] {
  const c: CodeInfo = CODES[code];
  return [c.before, c.core];
}

/** @internal A code's fix clause. */
export function fixOf(code: KnownCode): string {
  return CODES[code].fix;
}

/** @internal A code's docs anchor. */
export function anchorOf(code: string): string {
  return ERRORS_DOCS + code;
}

/** @internal A message the SDK writes itself: what failed, the code's fix and its docs anchor. */
export function messageFor(code: KnownCode, what: string): string {
  const w = what.trim().replace(/[.\s]+$/, "");
  return `wakeshift: ${w}. ${CODES[code].fix} ${anchorOf(code)}`;
}

/** One problem with an argument or a payload: what's wrong, and where. */
export interface Issue {
  message: string;
  path: (string | number)[];
}

/** Everything the SDK throws. Check with isWakeshiftError, which works across duplicate installs. */
export class WakeshiftError extends Error {
  /** The error's code: errors.json's list, shared by every SDK and the HTTP API. */
  readonly code: WakeshiftErrorCode;

  constructor(code: WakeshiftErrorCode, message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "WakeshiftError";
    this.code = code;
    Object.defineProperty(this, BRAND, { value: true });
  }
}

/**
 * Talking to Wakeshift failed: the core answered with an error (`status`,
 * `code`, `reason`, `requestId` when the answer carries one). Its subclass
 * APIConnectionError is the case where it didn't answer.
 */
export class APIError extends WakeshiftError {
  /** The HTTP status; null when Wakeshift didn't answer (APIConnectionError). */
  readonly status: number | null;
  /** Why, where the core says (callback_failed: what the callback did). */
  readonly reason?: string;
  /** The request's ID, when the answer carries one: give it to Wakeshift's support. */
  readonly requestId?: string;
  /** @internal The core's own message, without the prefix: what the built-in tools tell the agent. */
  declare readonly coreMessage: string;

  constructor(opts: { status: number | null; code: string; message: string; reason?: string; requestId?: string; cause?: unknown; text?: string }) {
    super(opts.code, opts.text ?? `wakeshift: ${opts.code || "error"} (${opts.status}): ${opts.message}`, { cause: opts.cause });
    this.name = "APIError";
    this.status = opts.status;
    if (opts.reason) this.reason = opts.reason;
    if (opts.requestId) this.requestId = opts.requestId;
    Object.defineProperty(this, "coreMessage", { value: opts.message, enumerable: false });
  }
}

/**
 * Wakeshift didn't answer: code `unreachable` (the connection failed; `cause`
 * is the transport's error) or `timeout` (the SDK's own timeout: the outcome
 * is unknown, so retry with the same id). An APIError with status null. A
 * caller's own abort is never this: the SDK rethrows the signal's reason.
 */
export class APIConnectionError extends APIError {
  declare readonly status: null;

  constructor(opts: { code: "unreachable" | "timeout"; message: string; cause?: unknown }) {
    super({ status: null, code: opts.code, message: opts.message, text: opts.message, cause: opts.cause });
    this.name = "APIConnectionError";
  }
}

/**
 * An emit's id was used within the last day for a different emit (another
 * event, arguments, recipients or payload): code id_reused, status 409.
 * Nothing was sent. A retry of the same emit isn't refused: it returns the
 * first emit's result with `duplicate: true`.
 */
export class IdReusedError extends APIError {
  constructor(opts: { message: string; requestId?: string; cause?: unknown }) {
    super({ status: 409, code: "id_reused", message: opts.message, requestId: opts.requestId, cause: opts.cause });
    this.name = "IdReusedError";
  }
}

/**
 * A mistake the SDK caught before sending anything: arguments or a payload
 * that don't match the event's schemas (`bad_arguments`, with `issues`),
 * configuration (`config`), a disabled client (`disabled`), and misuse
 * (`undeclared_subscribe`, `no_call`, `call_ended`, `identify_failed`, …).
 */
export class UsageError extends WakeshiftError {
  /** For bad_arguments: each problem, with its path. Empty otherwise. */
  readonly issues: Issue[];

  constructor(code: KnownCode, what: string, opts: { issues?: Issue[]; cause?: unknown } = {}) {
    super(code, messageFor(code, what), { cause: opts.cause });
    this.name = "UsageError";
    this.issues = opts.issues ?? [];
  }
}

// The class (or classes) an error with each code can be: what
// isWakeshiftError(err, code) narrows to.
type ClassOf<N> = N extends "UsageError" ? UsageError : N extends "APIConnectionError" ? APIConnectionError : N extends "IdReusedError" ? IdReusedError : N extends "APIError" ? APIError : never;
type CodeClass<C extends KnownCode> = ClassOf<(typeof CODES)[C]["before"]> | ClassOf<(typeof CODES)[C]["core"]>;

/**
 * Whether err is an error the SDK threw, of any install of it; with code,
 * whether it has that code, narrowed to the class that code comes as
 * (`"timeout"` is an APIConnectionError, `"config"` a UsageError).
 *
 *   if (isWakeshiftError(err, "id_reused")) …
 */
export function isWakeshiftError(err: unknown): err is WakeshiftError;
export function isWakeshiftError<C extends KnownCode>(err: unknown, code: C): err is CodeClass<C> & { readonly code: C };
export function isWakeshiftError(err: unknown, code?: KnownCode): boolean {
  if (!err || typeof err !== "object" || (err as Record<symbol, unknown>)[BRAND] !== true) return false;
  return code === undefined || (err as WakeshiftError).code === code;
}

/** @internal Whether err is the SDK's API error with this code (any install). */
export function hasCode(err: unknown, code: string): boolean {
  return isWakeshiftError(err) && err.code === code;
}

/** @internal The message of a Wakeshift error for an agent: the core's own words, or the SDK's message. */
export function detailOf(err: unknown): string {
  // No answer: what happened, without the developer's fix (an agent told
  // to retry could repeat work).
  if (err instanceof APIConnectionError) return err.code === "timeout" ? "Wakeshift didn't answer in time" : "Wakeshift couldn't be reached";
  if (err instanceof APIError) return err.coreMessage;
  if (isWakeshiftError(err)) return err.message.replace(/^wakeshift: /, "");
  return "an error";
}

/** @internal An error's type name, never its message (identify's and authorize's errors often carry tokens). */
export function errorType(err: unknown): string {
  if (err && typeof err === "object") {
    const name = (err as { name?: unknown }).name;
    if (typeof name === "string" && name) return name;
    const ctor = (err as { constructor?: { name?: unknown } }).constructor?.name;
    if (typeof ctor === "string" && ctor) return ctor;
  }
  return typeof err;
}
