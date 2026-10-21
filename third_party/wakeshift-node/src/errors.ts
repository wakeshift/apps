/**
 * An error response from Wakeshift: `code` is the API's error code (e.g.
 * "private_topic", "other_client", "unknown_subscription", "bad_request",
 * "not_headless"), `status` the HTTP status. A request that got no answer
 * has code "unreachable" and status 0.
 */
export class WakeshiftError extends Error {
  readonly status: number;
  readonly code: string;
  /** Wakeshift's own message, without the prefix. */
  readonly detail: string;
  /** Why, where the API says (callback_failed: what the callback did). */
  readonly reason?: string;

  constructor(status: number, code: string, message: string, options?: { cause?: unknown; reason?: string }) {
    super(status ? `wakeshift: ${code || "error"} (${status}): ${message}` : `wakeshift: ${message}`, options);
    this.name = "WakeshiftError";
    this.status = status;
    this.code = code;
    this.detail = message;
    if (options?.reason) this.reason = options.reason;
  }
}

/** Whether err is a WakeshiftError with code. */
export function isCode(err: unknown, code: string): boolean {
  return err instanceof WakeshiftError && err.code === code;
}
