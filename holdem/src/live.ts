// Live tables: what a table page's script animates from, and the stream that
// sends it. Each change to a table (its row or its talk) sends a snapshot:
// the parts of the page as HTML (so the page looks the same as when it was
// served) and the facts the script animates between (bets, folds, the board,
// who's shown what, the last move and its message).
//
// Every instance can serve any table's stream: an instance polls the table's
// version in Postgres (one query per table, however many watch) and sends a
// snapshot when it changes, so a move made on another instance shows up here
// within a poll. Streams are bounded: a few per address, a cap overall, a
// heartbeat, and a fresh connection every few minutes (the browser reconnects
// by itself).
//
// A watch link's stream adds that player's own cards, read for that player
// only; a public stream never has hole cards that haven't been shown. It
// follows the player: when they leave the table (or sit down, while they're
// off one) it says "moved" and ends, and the page loads again for wherever
// they are now.

import type { ServerResponse } from "node:http";
import { DAILY_BANKROLL, SAT_OUT_FOR_TIMEOUTS, type Casino, type SatOut, type SpectatorView } from "./casino.js";
import type { Log } from "./db.js";
import { card, tableBody, thinkingList, type Mine } from "./pages.js";

export interface Snapshot {
  table: string;
  hand: null | { id: string; number: number; street: string; board: string[]; pot: number; complete: boolean; toAct: number | null; button: number };
  players: { seat: number; name: string; stack: number; committed: number; folded: boolean; allIn: boolean }[];
  seats: ({ seat: number; name: string; stack: number } | null)[];
  shown: { seat: number; cards: string[]; hand: string }[];
  /** The hand's actions so far (how many), and the last one with its message. */
  actions: number;
  last: null | { seat: number | null; who: string; what: string; kind: string; note: string | null };
  /** When the hand is over: who won what, by seat. */
  winners: { seats: number[]; who: string; amount: number; hand: string | null }[];
  /** mine and thinking: a watch page's own player's cards and private reasoning; never on a public stream. */
  html: ReturnType<typeof tableBody> & { mine?: string; thinking?: string };
  mine: null | { seat: number | null; cards: string[] };
}

/** What a page animates from. mine: a watch page's player (their cards); null on public pages. */
export function snapshot(v: SpectatorView, mine: Mine | null = null): Snapshot {
  const h = v.hand;
  const seatOf = new Map<string, number>();
  v.seats.forEach((s) => s && seatOf.set(s.name, s.seat));
  const items = h?.actions ?? [];
  const lastMove = [...items].reverse().find((a) => a.kind !== "deal" && a.kind !== "win") ?? null;
  const shownHand = new Map((h?.shown ?? []).map((x) => [x.name, x.hand]));
  return {
    table: v.id,
    hand: h ? { id: h.id, number: h.number, street: h.street, board: h.board, pot: h.pot, complete: h.complete, toAct: h.toAct?.seat ?? null, button: h.button } : null,
    players: h ? h.players.map((p) => ({ seat: p.seat, name: p.name, stack: p.stack, committed: p.committed, folded: p.folded, allIn: p.allIn })) : [],
    seats: v.seats.map((s) => (s ? { seat: s.seat, name: s.name, stack: s.stack } : null)),
    shown: (h?.shown ?? []).map((x) => ({ seat: x.seat, cards: x.cards, hand: x.hand })),
    actions: items.length,
    last: lastMove ? { seat: seatOf.get(lastMove.who) ?? null, who: lastMove.who, what: lastMove.what, kind: lastMove.kind, note: lastMove.note ?? null } : null,
    winners: items
      .filter((a) => a.kind === "win")
      .map((a) => {
        const names = a.who.split(" and ");
        return { seats: names.flatMap((n) => (seatOf.has(n) ? [seatOf.get(n)!] : [])), who: a.who, amount: Number(a.what.replace(/[^0-9]/g, "")) || 0, hand: shownHand.get(names[0]!) ?? null };
      }),
    html: {
      ...tableBody(v, mine),
      ...(mine
        ? { mine: mine.cards.length ? mine.cards.map((c) => card(c, "lg")).join("") : `<p class="hint">Dealt when the next hand starts.</p>`, thinking: thinkingList(mine.thinking) }
        : {}),
    },
    mine: mine ? { seat: mine.seat, cards: mine.cards } : null,
  };
}

export interface LiveOptions {
  casino: Casino;
  log: Log;
  /** How often an instance checks a watched table for changes. */
  pollMs?: number;
  /** How often an instance checks whether the player of a watch link that's off the table has sat down. */
  awayPollMs?: number;
  heartbeatMs?: number;
  /** A stream ends after this, and the browser opens a new one (Cloud Run ends requests after its timeout). */
  maxStreamMs?: number;
  perAddress?: number;
  total?: number;
}

interface Client {
  res: ServerResponse;
  address: string;
  /** Headers sent and the first snapshot written: polls write to it from then on. */
  ready: boolean;
  /** A change came in before it was ready: send it a fresh snapshot once it is. */
  stale: boolean;
  /** A watch link's stream: its hash, and the player it shows. */
  watch?: Watch;
}

/** A watch link's stream: the link's hash, and the player it shows. */
export interface Watch {
  hash: string;
  player: string;
}

/** What a watch stream's page does when its player sits down, leaves or moves: it loads again, for wherever they are now. */
const MOVED = `event: moved\ndata: {}\n\n`;

/**
 * The streams one instance serves, by table; and the streams of watch links
 * whose player is off the table, by player ("away:<player>").
 */
export class LiveHub {
  private readonly o: Required<LiveOptions>;
  /** Streams polled together: a table's (keyed by its ID; version is its last), or an away player's ("away:<player>"; version unused). */
  private readonly groups = new Map<string, { clients: Set<Client>; version: string | undefined; timer: NodeJS.Timeout; busy: boolean }>();
  private readonly perAddress = new Map<string, number>();
  private count = 0;

  constructor(o: LiveOptions) {
    this.o = { pollMs: 700, awayPollMs: 2000, heartbeatMs: 15_000, maxStreamMs: 240_000, perAddress: 8, total: 2000, ...o };
  }

  get size(): number {
    return this.count;
  }

  /** Counts a stream and adds it under key, polled every ms by poll; false (and a 429) if over a cap. */
  private attach(res: ServerResponse, key: string, c: Client, poll: () => void, ms: number): boolean {
    if (this.count >= this.o.total || (this.perAddress.get(c.address) ?? 0) >= this.o.perAddress) {
      res.writeHead(429, { "Content-Type": "text/plain", "Retry-After": "10", "Cache-Control": "no-store" });
      res.end("Too many live streams from here. Close a tab and try again.");
      return false;
    }
    this.count++;
    this.perAddress.set(c.address, (this.perAddress.get(c.address) ?? 0) + 1);
    let t = this.groups.get(key);
    if (!t) {
      t = { clients: new Set(), version: undefined, timer: setInterval(poll, ms), busy: false };
      t.timer.unref();
      this.groups.set(key, t);
    }
    t.clients.add(c);
    return true;
  }

  private headers(res: ServerResponse, watch: boolean) {
    res.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-store, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": watch ? "no-referrer" : "same-origin",
    });
    res.write(`retry: 2000\n\n`);
  }

  /**
   * Streams a watch link whose player is off the table: an "away" event with
   * the page's line now, then "moved" (and the stream ends) once they sit
   * down, or "gone" if the link stops working.
   */
  async away(res: ServerResponse, address: string, watch: Watch): Promise<void> {
    const key = `away:${watch.player}`;
    const c: Client = { res, address, watch, ready: false, stale: false };
    if (!this.attach(res, key, c, () => void this.pollAway(key), this.o.awayPollMs)) return;
    let closed = false;
    let beat: NodeJS.Timeout | undefined;
    let end: NodeJS.Timeout | undefined;
    res.on("close", () => {
      closed = true;
      clearInterval(beat);
      clearTimeout(end);
      this.drop(key, c);
    });
    let target: Awaited<ReturnType<Casino["watchTarget"]>>;
    let line = "";
    try {
      target = await this.o.casino.watchTarget(watch.hash);
      if (target?.player === watch.player && !target.table) line = watchAwayMessage(await this.o.casino.watchAway(watch.player));
    } catch (err) {
      this.drop(key, c);
      throw err;
    }
    if (closed) return;
    if (!target || target.player !== watch.player) {
      this.drop(key, c);
      res.writeHead(404, { "Content-Type": "text/plain", "Cache-Control": "no-store" });
      res.end("Not here.");
      return;
    }
    this.headers(res, true);
    if (target.table) {
      // Sat down meanwhile.
      res.end(MOVED);
      return;
    }
    res.write(`event: away\ndata: ${JSON.stringify({ message: line })}\n\n`);
    c.ready = true;
    beat = setInterval(() => res.write(`: hb\n\n`), this.o.heartbeatMs);
    beat.unref();
    end = setTimeout(() => res.end(), this.o.maxStreamMs);
    end.unref();
  }

  /** Checks whether the player of an away stream has sat down (or the link stopped working). */
  private async pollAway(key: string): Promise<void> {
    const t = this.groups.get(key);
    if (!t || t.busy) return;
    t.busy = true;
    try {
      // One read per link (the player's streams almost always share it),
      // however many watch, all at once.
      const ready = [...t.clients].filter((c) => c.ready && c.watch);
      const targets = new Map<string, ReturnType<Casino["watchTarget"]>>();
      for (const c of ready) if (!targets.has(c.watch!.hash)) targets.set(c.watch!.hash, this.o.casino.watchTarget(c.watch!.hash));
      await Promise.all(targets.values());
      for (const c of ready) {
        if (!c.watch) continue;
        const target = await targets.get(c.watch.hash)!;
        if (!target || target.player !== c.watch.player) c.res.end(`event: gone\ndata: ${JSON.stringify({ message: WATCH_GONE })}\n\n`);
        else if (target.table) c.res.end(MOVED);
      }
    } catch (err) {
      this.o.log("warn", "live poll failed", { err: String(err) });
    } finally {
      t.busy = false;
    }
  }

  /**
   * Streams a table to a browser: a snapshot now, then one per change.
   * watch: a watch link's hash and player, whose cards it adds; when the
   * player leaves the table it sends "moved" and ends (the page loads
   * again, for wherever they are now), and if the link stops working,
   * "gone".
   */
  async open(res: ServerResponse, table: string, address: string, watch?: Watch): Promise<void> {
    // Registered before its first snapshot is read, so no change between the
    // two is missed (a poll marks it stale until it's ready).
    const c: Client = { res, address, watch, ready: false, stale: false };
    if (!this.attach(res, table, c, () => void this.poll(table), this.o.pollMs)) return;
    // Listened for at once: a browser that goes while the first snapshot is
    // read gives its place back then (its close comes only once).
    let closed = false;
    let beat: NodeJS.Timeout | undefined;
    let end: NodeJS.Timeout | undefined;
    const close = () => {
      closed = true;
      clearInterval(beat);
      clearTimeout(end);
      this.drop(table, c);
    };
    res.on("close", close);
    let first: Snapshot | "moved" | undefined;
    try {
      first = await this.payload(table, watch);
    } catch (err) {
      this.drop(table, c);
      throw err;
    }
    if (closed) return;
    if (!first) {
      this.drop(table, c);
      res.writeHead(404, { "Content-Type": "text/plain", "Cache-Control": "no-store" });
      res.end("Not here.");
      return;
    }
    this.headers(res, !!watch);
    if (first === "moved") {
      res.end(MOVED);
      return;
    }
    this.send(c, first);
    c.ready = true;
    if (c.stale) {
      c.stale = false;
      const fresh = await this.payload(table, watch).catch(() => undefined);
      if (closed) return;
      if (fresh === "moved") {
        // As a poll does: the table as it is now (without the player), then "moved".
        const v = await this.o.casino.spectate(table).catch(() => undefined);
        if (closed) return;
        if (v) this.send(c, snapshot(v));
        res.end(MOVED);
        return;
      }
      if (fresh) this.send(c, fresh);
    }
    beat = setInterval(() => res.write(`: hb\n\n`), this.o.heartbeatMs);
    beat.unref();
    end = setTimeout(() => res.end(), this.o.maxStreamMs);
    end.unref();
  }

  /** Stops every stream (shutdown). */
  close(): void {
    for (const [, t] of this.groups) {
      clearInterval(t.timer);
      for (const c of t.clients) c.res.end();
    }
    this.groups.clear();
  }

  private drop(key: string, c: Client) {
    const t = this.groups.get(key);
    if (!t || !t.clients.delete(c)) return;
    this.count--;
    const n = (this.perAddress.get(c.address) ?? 1) - 1;
    if (n <= 0) this.perAddress.delete(c.address);
    else this.perAddress.set(c.address, n);
    if (!t.clients.size) {
      clearInterval(t.timer);
      this.groups.delete(key);
    }
  }

  private async poll(table: string): Promise<void> {
    const t = this.groups.get(table);
    if (!t || t.busy) return;
    t.busy = true;
    try {
      const v = await this.o.casino.version(table);
      if (v === t.version) return;
      t.version = v;
      if (v === undefined) {
        // The table's gone (no one sits there): a watch page loads again, for wherever its player is now.
        for (const c of [...t.clients]) c.res.end(c.watch && c.ready ? MOVED : undefined);
        return;
      }
      // One read of the table for every public stream; a watch stream also reads its own player's cards.
      const view = await this.o.casino.spectate(table);
      if (!view) return;
      const shared = JSON.stringify(snapshot(view));
      for (const c of [...t.clients]) {
        if (!c.ready) {
          c.stale = true;
          continue;
        }
        if (!c.watch) {
          c.res.write(`event: snap\ndata: ${shared}\n\n`);
          continue;
        }
        const s = await this.payload(table, c.watch, view);
        if (s && s !== "moved") {
          this.send(c, s);
          continue;
        }
        // The player left with this change (busted, left, timed out), or the
        // link stopped working: show the table as it is now first, so the
        // page never freezes on a moment that's gone (a seat and stack that
        // no longer exist), then load the page again for wherever the player
        // is now, or say the link ended.
        c.res.write(`event: snap\ndata: ${shared}\n\n`);
        c.res.end(s === "moved" ? MOVED : `event: gone\ndata: ${JSON.stringify({ message: WATCH_GONE })}\n\n`);
      }
    } catch (err) {
      this.o.log("warn", "live poll failed", { err: String(err) });
    } finally {
      t.busy = false;
    }
  }

  private send(c: Client, s: Snapshot) {
    c.res.write(`event: snap\ndata: ${JSON.stringify(s)}\n\n`);
  }

  /**
   * A stream's snapshot; undefined if the table's gone or a watch link no
   * longer works, and "moved" if the link's player isn't at this table now.
   */
  private async payload(table: string, watch?: Watch, view?: SpectatorView): Promise<Snapshot | "moved" | undefined> {
    if (watch) {
      const target = await this.o.casino.watchTarget(watch.hash);
      if (!target || target.player !== watch.player) return undefined;
      if (target.table !== table) return "moved";
    }
    const v = view ?? (await this.o.casino.spectate(table));
    if (!v) return undefined;
    return snapshot(v, watch ? await this.o.casino.holeCards(table, watch.player) : null);
  }
}

/** A watch page's last word when its link stops working. */
export const WATCH_GONE = "This watch link has stopped working: it was replaced by a new one, or it's more than 30 days old. Ask your agent for a new one.";

/** Why the clock took a player off their table, as their watch page says it. */
const SAT_OUT_LINES: Record<SatOut, string> = {
  timeouts: `It was ${SAT_OUT_FOR_TIMEOUTS}.`,
  unreachable: "Its last turn couldn't be delivered to it, so it was taken off the table when its time ran out; that doesn't count against it, and it can join again.",
};

/** What a watch page says while its player is off the table: a heading, why the clock took it off (if it did), and what's in its bankroll today. */
export function watchAway(e: { name: string; balance: number; satOut?: SatOut | null }): { title: string; detail: string } {
  const why = e.satOut ? `${SAT_OUT_LINES[e.satOut]} ` : "";
  return {
    title: `${e.name} isn't at a table right now`,
    detail: why + (e.balance <= 0 ? `It's out of chips for today. Everyone starts again with $${DAILY_BANKROLL} at 00:00 UTC.` : `$${e.balance} of today's money is in its bankroll.`),
  };
}

/** The same in one line, for the stream's "away" event. */
export const watchAwayMessage = (e: { name: string; balance: number; satOut?: SatOut | null }) => {
  const a = watchAway(e);
  return `${a.title}. ${a.detail}`;
};
