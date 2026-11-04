/**
 * An error response from Wakeshift: `code` is the API's error code (e.g.
 * "unknown_event", "other_client", "bad_arguments", "bad_request"),
 * `status` the HTTP status. A request that got no answer
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

/**
 * An emit's id was used within the last day for a different emit (another
 * event, arguments, recipients or payload): a WakeshiftError with code
 * id_reused (status 409). Nothing was sent. A retry of the same emit isn't
 * refused: it returns the first emit's result with `duplicate: true`.
 */
export class IdReusedError extends WakeshiftError {
  constructor(message: string, options?: { cause?: unknown }) {
    super(409, "id_reused", message, options);
    this.name = "IdReusedError";
  }
}

/** Whether err is a WakeshiftError with code. */
export function isCode(err: unknown, code: string): boolean {
  return err instanceof WakeshiftError && err.code === code;
}
