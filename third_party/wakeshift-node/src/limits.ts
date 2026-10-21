// Per-address limits for unauthenticated endpoints (GuestAuth's register
// and token): who a request is from, behind trusted proxies, and how many
// requests each address may make. In memory, per process.

import type { IncomingMessage } from "node:http";
import { isIP } from "node:net";

// LIMITER_KEYS bounds a limiter's memory: past it, a new address is refused
// until old ones expire (the endpoints are unauthenticated, and IPv6 makes
// addresses cheap).
const LIMITER_KEYS = 100_000;

/** A rate limiter: at most max events per window (ms) for each key, in memory, pruning idle keys as it goes. A negative max turns it off. */
export class RateLimiter {
  private readonly seen = new Map<string, number[]>();
  private pruned = 0;

  constructor(
    readonly max: number,
    readonly window: number,
    readonly keys = LIMITER_KEYS,
  ) {}

  allow(key: string, now: number): boolean {
    if (this.max < 0) return true;
    key = limitKey(key);
    const cut = now - this.window;
    const prune = () => {
      for (const [k, ts] of this.seen) if (ts.length === 0 || ts[ts.length - 1] <= cut) this.seen.delete(k);
      this.pruned = now;
    };
    if (now - this.pruned > this.window) prune();
    if (!this.seen.has(key) && this.seen.size >= this.keys) {
      prune();
      if (this.seen.size >= this.keys) return false;
    }
    const ts = (this.seen.get(key) ?? []).filter((t) => t > cut);
    if (ts.length >= this.max) {
      this.seen.set(key, ts);
      return false;
    }
    ts.push(now);
    this.seen.set(key, ts);
    return true;
  }
}

/** What an address counts as for the limits: an IPv6 address's /64 (one host usually has a whole /64), anything else as it is. */
export function limitKey(addr: string): string {
  const a = addr.replace(/%.*$/, "");
  if (isIP(a) !== 6 || /^::ffff:\d+\.\d+\.\d+\.\d+$/i.test(a)) return addr;
  const groups = expandIPv6(a);
  return groups ? groups.slice(0, 4).join(":") + "::/64" : addr;
}

// expandIPv6 is an IPv6 address's eight groups (a dotted IPv4 tail as two).
function expandIPv6(a: string): string[] | null {
  let s = a.toLowerCase();
  const v4 = /(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(s);
  if (v4) {
    const b = v4.slice(1).map(Number);
    s = s.slice(0, v4.index) + ((b[0] << 8) | b[1]).toString(16) + ":" + ((b[2] << 8) | b[3]).toString(16);
  }
  const halves = s.split("::");
  if (halves.length > 2) return null;
  const head = halves[0] ? halves[0].split(":") : [];
  const tail = halves.length === 2 && halves[1] ? halves[1].split(":") : [];
  const fill = halves.length === 2 ? 8 - head.length - tail.length : 0;
  const all = [...head, ...Array(Math.max(fill, 0)).fill("0"), ...tail];
  if (all.length !== 8) return null;
  return all.map((g) => parseInt(g, 16).toString(16));
}

/**
 * The request's client address: with hops trusted proxies in front (each
 * appending to X-Forwarded-For), the entry the outermost one added; entries
 * before it are the client's own claims.
 */
export function clientAddr(req: IncomingMessage, hops: number): string {
  // The first X-Forwarded-For header only, as Go's Header.Get reads it.
  const distinct = (req as IncomingMessage & { headersDistinct?: Record<string, string[] | undefined> }).headersDistinct?.["x-forwarded-for"];
  const raw = req.headers["x-forwarded-for"];
  const f = distinct?.[0] ?? (Array.isArray(raw) ? raw[0] : raw);
  if (f && hops > 0) {
    // Only an entry the proxies added: with fewer entries than hops, or one
    // that isn't an address, the header isn't the proxies' (a request that
    // reached the app some other way), so it's ignored.
    const parts = f.split(",");
    if (parts.length >= hops) {
      const ip = parts[parts.length - hops].trim();
      if (isIP(ip)) return normalizeAddr(ip);
    }
  }
  return normalizeAddr(req.socket?.remoteAddress ?? "");
}

// normalizeAddr writes an IPv4 address a dual-stack socket reports as
// ::ffff:a.b.c.d as a.b.c.d, as Go does.
function normalizeAddr(a: string): string {
  const m = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(a);
  return m ? m[1] : a;
}
