// event.watch (test/watch.json): for a vendor whose API has no usable
// webhook, a bounded background check that emits once. It calls check
// until check returns a payload, then emits it, with backoff, a hard
// timeout, a cap on checks running at once, and one emit id across retries.
// It runs in this process only: a restart or a serverless freeze loses it.
//
// And an event's onSubscribe: run once per new subscription, whichever path
// made it, in the background.

import { randomUUID } from "node:crypto";
import { APIConnectionError, APIError, IdReusedError, UsageError, errorType } from "./errors.js";
import { durationSeconds, findCall } from "./toolcall.js";
import type { Duration, Emitted, Wakeshift } from "./wakeshift.js";
import type { WakeEvent } from "./events.js";

/** How event.watch checks and emits. Every field is optional. */
export interface WatchOptions {
  /** How long to wait after a check that found the work still running. Default "30s"; at least 1 second. */
  every?: Duration;
  /** When to give up, counted from the start, checks and emit retries included. Default "1h"; at least every. */
  timeout?: Duration;
  /** Stop after this many check errors in a row. Default 5. */
  maxErrors?: number;
  /** Whether a check's error is final (the job is gone, the credential was revoked): the watch stops at once. */
  isFinal?: (err: unknown) => boolean;
  /**
   * The emit's id (emit's `id`): pass the one your webhook handler uses for
   * the same occurrence, so a webhook and a watch emit it once. Default: one
   * id per watch, the same on every retry of its emit.
   */
  id?: string;
  /** Ends the watch, as watch.stop() does. */
  signal?: AbortSignal;
}

/** What a watch's check gets: a signal that aborts when the watch stops or times out, and which check this is (0 first). */
export interface WatchCheckContext {
  signal: AbortSignal;
  attempt: number;
}

/** A watch's check: the payload when the work is done, or undefined to keep waiting. It may throw; see maxErrors and isFinal. */
export type WatchCheck<P> = (ctx: WatchCheckContext) => P | undefined | null | Promise<P | undefined | null>;

/**
 * How a watch ended:
 *   - emitted: it emitted (emitted is null when another emit with the
 *     same id got there first, a webhook's);
 *   - timeout: it reached its timeout without a payload;
 *   - stopped: watch.stop(), wake.close() or its signal;
 *   - skipped: nothing ran (a disabled client, or the tool call's
 *     subscription for these arguments came back unavailable or disabled);
 *   - failed: reason check (an error isFinal marked final), errors
 *     (maxErrors in a row) or emit (an emit error retrying wouldn't help).
 */
export type WatchResult =
  | { outcome: "emitted"; emitted: Emitted | null }
  | { outcome: "timeout" | "stopped" | "skipped" }
  | { outcome: "failed"; reason: "check" | "errors" | "emit"; error: unknown };

/** A running watch (event.watch). A second watch of the same event and arguments while one runs returns the running one. */
export interface Watch {
  /** Resolves (never rejects) when the watch ends. */
  readonly done: Promise<WatchResult>;
  /** Stops the watch: done resolves to { outcome: "stopped" }. */
  stop(): void;
}

/**
 * What an event's onSubscribe gets: the new subscription's arguments, its
 * user, and the path that made it. Ctx is the type of the context your
 * tools pass call.subscribe: annotate onSubscribe's parameter with it,
 * `(s: Subscribed<Args, ApiKey>) => …`, to read s.context without a cast.
 */
export interface Subscribed<Args = Record<string, unknown>, Ctx = unknown> {
  args: Args;
  /** The subscriber's user (attach's identify), if any. */
  user: string | undefined;
  /** tool: one of the app's tools (call.subscribe); subscribe_events; native: an MCP Events client's events/subscribe or events/poll. */
  via: "tool" | "subscribe_events" | "native";
  /** The subscribing tool's name, for via tool. */
  tool: string | undefined;
  /**
   * The context the subscribing call passed call.subscribe ({ context }),
   * for via tool: that call's own subscription, or one made with the
   * voucher it handed out while this process still holds it (a redemption
   * on another instance, or after a restart, has none). undefined
   * otherwise (subscribe_events, native). Without it, a watch can't run
   * with the caller's credentials: log that, or fall back to your status tool.
   */
  context?: Ctx;
}

/** @internal Timing knobs tests turn: every delay and timeout is multiplied by scale; jitter off is exact. */
export const watchTiming = { scale: 1, jitter: true };

/** The most checks one client runs at once; the others wait their turn. */
export const MAX_CONCURRENT_CHECKS = 10;
const DEFAULT_EVERY = 30;
const DEFAULT_TIMEOUT = 3600;
const DEFAULT_MAX_ERRORS = 5;

/** @internal How long a watch waits before its next check, in seconds (test/watch.json delays). */
export function watchDelay(every: number, errorsInARow: number): number {
  if (errorsInARow <= 0) return every;
  return Math.min(every * 2 ** errorsInARow, Math.max(every, 300));
}

// One client's watches, check slots and onSubscribe work.
class Registry {
  running = new Map<string, WatchImpl>();
  active = 0;
  queue: (() => void)[] = [];
  pending = new Set<Promise<unknown>>();
  closed = false;

  async acquire(signal: AbortSignal): Promise<boolean> {
    if (this.active < MAX_CONCURRENT_CHECKS) {
      this.active++;
      return true;
    }
    return new Promise<boolean>((resolve) => {
      const go = () => {
        signal.removeEventListener("abort", give);
        this.active++;
        resolve(true);
      };
      const give = () => {
        this.queue = this.queue.filter((f) => f !== go);
        resolve(false);
      };
      this.queue.push(go);
      signal.addEventListener("abort", give, { once: true });
    });
  }

  release(): void {
    this.active--;
    const next = this.queue.shift();
    if (next) next();
  }
}

const registries = new WeakMap<Wakeshift, Registry>();

// unref lets the process exit while a watch only waits: a watch never keeps
// a server alive (a stdio server whose client went away exits).
function unref<T>(t: T): T {
  (t as { unref?: () => void } | undefined)?.unref?.();
  return t;
}

function registryOf(wake: Wakeshift): Registry {
  let r = registries.get(wake);
  if (!r) registries.set(wake, (r = new Registry()));
  return r;
}

/** @internal wake.close(): stops every watch, and waits for them and for onSubscribe work in flight. */
export async function closeWatches(wake: Wakeshift): Promise<void> {
  const r = registryOf(wake);
  r.closed = true;
  const watches = [...r.running.values()];
  for (const w of watches) w.stop();
  await Promise.allSettled([...watches.map((w) => w.done), ...r.pending]);
}

/**
 * @internal The testing client's close: waits for onSubscribe work, then for
 * each watch's check in flight and the emit it leads to, and only then
 * stops every watch.
 */
export async function drainWatches(wake: Wakeshift): Promise<void> {
  const r = registryOf(wake);
  await Promise.allSettled([...r.pending]);
  await Promise.allSettled([...r.running.values()].map((w) => w.inflight));
}

/** @internal Waits for the onSubscribe work in flight (the testing module's finish). */
export async function onSubscribeSettled(wake: Wakeshift): Promise<void> {
  await Promise.allSettled([...registryOf(wake).pending]);
}

/** @internal The canonical key of an event and its arguments. */
export function watchKey(name: string, args: unknown): string {
  const norm = (v: unknown): unknown =>
    Array.isArray(v) ? v.map(norm) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v as object).sort().map((k) => [k, norm((v as Record<string, unknown>)[k])])) : v;
  return name + "\u0000" + JSON.stringify(norm(args ?? {}));
}

// retryable is whether an emit error may go away by itself: no answer, a
// rate limit or Wakeshift's own failure.
function retryable(err: unknown): boolean {
  if (err instanceof APIConnectionError) return true;
  if (err instanceof APIError) return err.status === 429 || err.code === "rate_limited" || (typeof err.status === "number" && err.status >= 500);
  return false;
}

const SKIPPED: Watch = Object.freeze({ done: Promise.resolve<WatchResult>({ outcome: "skipped" }), stop() {} });

/** @internal A watch that runs nothing (outcome skipped). */
export function skippedWatch(): Watch {
  return SKIPPED;
}

class WatchImpl implements Watch {
  readonly done: Promise<WatchResult>;
  /** @internal Settles when the check in flight, and the emit it leads to, are done (drainWatches). */
  inflight: Promise<void> = Promise.resolve();
  private readonly ctl = new AbortController();
  private why: "stopped" | "timeout" | null = null;

  constructor(
    private readonly wake: Wakeshift,
    private readonly event: WakeEvent,
    private readonly args: Record<string, unknown>,
    private readonly check: WatchCheck<unknown>,
    private readonly o: { every: number; timeout: number; maxErrors: number; isFinal?: (err: unknown) => boolean; id: string; to?: unknown; signal?: AbortSignal },
    private readonly onEnd: () => void,
  ) {
    this.done = this.run().finally(onEnd);
  }

  stop(): void {
    this.abort("stopped");
  }

  private abort(why: "stopped" | "timeout"): void {
    if (this.ctl.signal.aborted) return;
    this.why = why;
    this.ctl.abort(Object.assign(new Error(`the watch ${why === "timeout" ? "timed out" : "was stopped"}`), { name: "AbortError" }));
  }

  private ms(seconds: number): number {
    const jitter = watchTiming.jitter ? 1 + (Math.random() * 0.2 - 0.1) : 1;
    return seconds * 1000 * watchTiming.scale * jitter;
  }

  // sleep waits seconds, or until the watch is aborted (false then).
  private sleep(seconds: number): Promise<boolean> {
    const signal = this.ctl.signal;
    if (signal.aborted) return Promise.resolve(false);
    return new Promise((resolve) => {
      const t = unref(
        setTimeout(() => {
          signal.removeEventListener("abort", stop);
          resolve(true);
        }, this.ms(seconds)),
      );
      const stop = () => {
        clearTimeout(t);
        resolve(false);
      };
      signal.addEventListener("abort", stop, { once: true });
    });
  }

  private ended(): WatchResult {
    if (this.why === "timeout") {
      this.wake.log.log("watch_timeout", `a watch of ${this.event.name} reached its timeout without a payload`, { event: this.event.name });
      return { outcome: "timeout" };
    }
    return { outcome: "stopped" };
  }

  private failed(reason: "check" | "errors" | "emit", error: unknown): WatchResult {
    if (reason === "check") {
      // isFinal marked the error final: the watch stopped as asked, nothing to fix.
      this.wake.log.log("watch_stopped", `a watch of ${this.event.name} stopped: its check's error was marked final (${errorType(error)})`, { event: this.event.name, error_type: errorType(error) });
      return { outcome: "failed", reason, error };
    }
    const why = reason === "errors" ? "its check failed too many times in a row" : "its emit failed and retrying wouldn't help";
    this.wake.log.log("watch_failed", `a watch of ${this.event.name} ended: ${why} (${errorType(error)})`, { event: this.event.name, reason, error_type: errorType(error) });
    return { outcome: "failed", reason, error };
  }

  private async run(): Promise<WatchResult> {
    const timer = unref(setTimeout(() => this.abort("timeout"), this.o.timeout * 1000 * watchTiming.scale));
    const outside = this.o.signal;
    const onOutside = () => this.abort("stopped");
    if (outside?.aborted) this.abort("stopped");
    outside?.addEventListener("abort", onOutside, { once: true });
    const reg = registryOf(this.wake);
    let idle: () => void = () => undefined;
    try {
      let errors = 0;
      for (let attempt = 0; ; attempt++) {
        idle();
        this.inflight = new Promise<void>((resolve) => (idle = resolve));
        if (this.ctl.signal.aborted) return this.ended();
        if (!(await reg.acquire(this.ctl.signal))) return this.ended();
        let payload: unknown;
        let failure: { err: unknown } | undefined;
        try {
          payload = await this.check({ signal: this.ctl.signal, attempt });
        } catch (err) {
          failure = { err };
        } finally {
          reg.release();
        }
        if (this.ctl.signal.aborted) return this.ended();
        let delay: number;
        if (failure) {
          if (this.o.isFinal?.(failure.err)) return this.failed("check", failure.err);
          errors++;
          if (errors >= this.o.maxErrors) return this.failed("errors", failure.err);
          delay = watchDelay(this.o.every, errors);
        } else if (payload === undefined || payload === null) {
          errors = 0;
          delay = this.o.every;
        } else {
          return await this.emit(payload);
        }
        idle();
        if (!(await this.sleep(delay))) return this.ended();
      }
    } finally {
      idle();
      clearTimeout(timer);
      outside?.removeEventListener("abort", onOutside);
    }
  }

  // emit sends the payload once, retrying what may go away by itself, with
  // the same id every time.
  private async emit(payload: unknown): Promise<WatchResult> {
    for (let tries = 1; ; tries++) {
      try {
        const opts: Record<string, unknown> = { id: this.o.id, signal: this.ctl.signal };
        if (this.o.to !== undefined) opts.to = this.o.to;
        const emitted = await (this.event as WakeEvent<string, any, any, any>).emit(this.args, payload as never, opts as never);
        return { outcome: "emitted", emitted };
      } catch (err) {
        if (this.ctl.signal.aborted) return this.ended();
        if (err instanceof IdReusedError) return { outcome: "emitted", emitted: null };
        if (!retryable(err)) return this.failed("emit", err);
        if (!(await this.sleep(watchDelay(this.o.every, tries)))) return this.ended();
      }
    }
  }
}

/** @internal event.watch. */
export function startWatch(wake: Wakeshift, event: WakeEvent, rawArgs: unknown, check: unknown, opts: (WatchOptions & { to?: unknown }) | undefined): Watch {
  const o = opts ?? {};
  if (typeof o !== "object") throw new UsageError("bad_arguments", `watch's options are an object: { every, timeout, maxErrors, isFinal, id, to, signal }`);
  if (typeof check !== "function") throw new UsageError("bad_arguments", `watch of ${event.name} needs a check function`);
  const every = o.every === undefined ? DEFAULT_EVERY : durationSeconds(o.every, "every");
  const timeout = o.timeout === undefined ? DEFAULT_TIMEOUT : durationSeconds(o.timeout, "timeout");
  const maxErrors = o.maxErrors === undefined ? DEFAULT_MAX_ERRORS : o.maxErrors;
  if (!(every >= 1)) throw new UsageError("bad_arguments", "watch's every is at least 1 second");
  if (!(timeout > 0)) throw new UsageError("bad_arguments", "watch's timeout is over 0");
  if (timeout < every) throw new UsageError("bad_arguments", "watch's timeout is at least its every");
  if (!(Number.isInteger(maxErrors) && maxErrors >= 1)) throw new UsageError("bad_arguments", "watch's maxErrors is a whole number, 1 or more");
  if (o.isFinal !== undefined && typeof o.isFinal !== "function") throw new UsageError("bad_arguments", "watch's isFinal is a function, (err) => boolean");
  if (o.id !== undefined && (typeof o.id !== "string" || !o.id.trim())) throw new UsageError("bad_arguments", "watch's id is a non-empty string");
  const args = event.parseArgs(rawArgs) as Record<string, unknown>;
  if (event.personal && o.to === undefined) throw new UsageError("bad_arguments", `${event.name} is personal: pass { to }, the subscriber the watch emits to`);
  if (!event.personal && o.to !== undefined) throw new UsageError("bad_arguments", `${event.name} isn't personal: its watch takes no to`);
  if (!wake.enabled) return SKIPPED;
  const reg = registryOf(wake);
  if (reg.closed) return SKIPPED;
  const key = watchKey(event.name, args);
  const running = reg.running.get(key);
  if (running) return running;
  // Inside a tool call: its subscription for these arguments came back
  // unavailable or disabled, so nothing would receive the emit.
  const scope = findCall(wake);
  const state = scope?.stateOf(key);
  if (state === "unavailable" || state === "disabled") return SKIPPED;
  const w = new WatchImpl(wake, event, args, check as WatchCheck<unknown>, { every, timeout, maxErrors, isFinal: o.isFinal, id: o.id ?? "watch_" + randomUUID(), to: o.to, signal: o.signal }, () => {
    if (reg.running.get(key) === w) reg.running.delete(key);
  });
  reg.running.set(key, w);
  // A watch for a subscription this call made stops if the call fails.
  if (scope && state === "created") scope.watches.push(w);
  return w;
}

/** @internal Runs an event's onSubscribe in the background; a throw logs on_subscribe_failed. */
export function runOnSubscribe(wake: Wakeshift, event: WakeEvent, fn: ((s: Subscribed<any>) => unknown) | undefined, s: Subscribed<any>, runIn?: <R>(fn: () => R) => R): void {
  if (!fn || !wake.enabled) return;
  const reg = registryOf(wake);
  // In the async context the subscribe ran in (a tool's call), if given.
  const run = runIn ? () => runIn(() => fn(s)) : () => fn(s);
  const p = Promise.resolve()
    .then(run)
    .catch((err) => {
      wake.log.log("on_subscribe_failed", `${event.name}'s onSubscribe failed (${errorType(err)})`, { event: event.name, error_type: errorType(err) });
    });
  reg.pending.add(p);
  void p.finally(() => reg.pending.delete(p));
}
