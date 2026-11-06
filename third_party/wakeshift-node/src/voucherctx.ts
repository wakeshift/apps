// What a voucher's redemption hands onSubscribe: the context
// (call.subscribe's { context }) of the tool call that handed the voucher
// out, while this process still holds it. Kept per client, in memory only,
// by the SHA-256 of the voucher (never the voucher itself), for at most as
// long as a voucher lives (four hours), at most MAX_ENTRIES of them (the
// oldest go first), and taken out by the redemption that creates the
// subscription. A redemption on another instance, or after a restart, gets
// none. No timer: expired entries go when the map is next touched.

import { createHash } from "node:crypto";

/** @internal How long a voucher can be redeemed (the core's VoucherTTL). */
export const VOUCHER_TTL_MS = 4 * 3600_000;
/** @internal The most voucher contexts one client holds. */
export const MAX_VOUCHER_CONTEXTS = 1000;

/** @internal The clock (tests replace it). */
export const voucherClock = { now: (): number => Date.now() };

// The core's line for a handed-out voucher (internal/appapi/vouchers.go
// voucherLine). The arguments are greedy, so the real voucher, which the
// core writes last on the line, wins over anything that looks like one
// inside an argument's value.
const LINE_RE = /To be woken by (\S+) with (\{.*\}), subscribe to it \(MCP Events\) with wakeshift_voucher set to (wsv_[a-z2-7]{32}) in its arguments\./;

type Entry = { context: unknown; expires: number };
const memories = new WeakMap<object, Map<string, Entry>>();

function hashOf(voucher: string): string {
  return createHash("sha256").update(voucher).digest("hex");
}

function canonical(v: unknown): string {
  const norm = (x: unknown): unknown =>
    Array.isArray(x) ? x.map(norm) : x && typeof x === "object" ? Object.fromEntries(Object.keys(x as object).sort().map((k) => [k, norm((x as Record<string, unknown>)[k])])) : x;
  return JSON.stringify(norm(v ?? {}));
}

function purge(m: Map<string, Entry>, now: number): void {
  for (const [k, e] of m) if (e.expires <= now) m.delete(k);
}

/** @internal One subscribe of a call to a tools-only event, with its context. */
export interface VoucherLine {
  name: string;
  arguments: unknown;
  context: unknown;
}

/**
 * @internal Remembers, for each voucher in a call's addition text, the
 * context of the subscribe it was handed out for (by event and arguments).
 * Only subscribes that passed a context are remembered.
 */
export function rememberVouchers(owner: object, text: string | undefined, lines: readonly VoucherLine[]): void {
  if (!text || !lines.some((l) => l.context !== undefined)) return;
  let m = memories.get(owner);
  const now = voucherClock.now();
  for (const raw of text.split("\n")) {
    const match = LINE_RE.exec(raw);
    if (!match) continue;
    let args: unknown;
    try {
      args = JSON.parse(match[2]!);
    } catch {
      continue;
    }
    const want = canonical(args);
    const line = lines.find((l) => l.name === match[1] && canonical(l.arguments) === want);
    if (!line || line.context === undefined) continue;
    if (!m) memories.set(owner, (m = new Map()));
    purge(m, now);
    const key = hashOf(match[3]!);
    m.delete(key);
    m.set(key, { context: line.context, expires: now + VOUCHER_TTL_MS });
    while (m.size > MAX_VOUCHER_CONTEXTS) m.delete(m.keys().next().value!);
  }
}

/** @internal The context remembered for exactly this voucher, taken out (a redemption that created its subscription); undefined if none. */
export function takeVoucherContext(owner: object, voucher: string): unknown {
  const m = memories.get(owner);
  if (!m) return undefined;
  purge(m, voucherClock.now());
  const key = hashOf(voucher);
  const e = m.get(key);
  if (!e) return undefined;
  m.delete(key);
  return e.context;
}

/** @internal Forgets every voucher context (wake.close()). */
export function forgetVoucherContexts(owner: object): void {
  memories.delete(owner);
}

/** @internal How many voucher contexts a client holds, and their keys (tests: never a voucher). */
export function voucherContextKeys(owner: object): string[] {
  return [...(memories.get(owner)?.keys() ?? [])];
}
